// Command mpa-bridge serves JVM metrics of Pelican Minecraft servers to the
// MPA panel plugin. See docs/API.md.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/assets"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/api"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/attach"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/config"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/profile"
)

// Version is set at build time with -ldflags "-X main.Version=…".
var Version = "1.1.0"

func main() {
	cfgPath := flag.String("config", config.DefaultBridgePath, "path of mpa-bridge.yml")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("mpa-bridge", Version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.Allowed) == 0 {
		log.Warn("allowed_ips is empty: every address may connect (the token is still required)")
	}
	docker := container.NewDocker(cfg.DockerSocket)
	proc := container.Proc{Root: cfg.ProcRoot}
	attacher := attach.New(api.MaxAttachOutput)
	srv := &api.Server{
		Version:    Version,
		Creds:      config.NewCredentials(cfgPath, cfg),
		Allowed:    cfg.Allowed,
		Docker:     docker,
		Proc:       proc,
		Attach:     attacher,
		TmpfsBytes: cfg.TmpfsSizeBytes,
		TLS:        cfg.API.SSL.Enabled,
		Limits: api.NodeLimits{MaxConcurrentProfiles: cfg.MaxConcurrentProfiles, MaxProfileSeconds: cfg.MaxProfileSeconds,
			MaxTickMonitors: cfg.MaxTickMonitors},
		Log: log,
	}
	srv.Profiles = &profile.Manager{
		MaxConcurrent: cfg.MaxConcurrentProfiles,
		Lib:           assets.AsyncProfiler,
		Loader:        attacher,
		ProcRoot:      cfg.ProcRoot,
		CPUTicks:      proc.CPUTicks,
		Check:         srv.CheckServer,
		Log:           log,
	}
	srv.Ticks = &profile.Ticks{
		Max:       cfg.MaxTickMonitors,
		Profiles:  srv.Profiles,
		ThreadID:  proc.ThreadID,
		HostTID:   proc.HostTID,
		SchedStat: proc.SchedStat,
		Jcmd:      attacher.Jcmd,
		Log:       log,
	}
	srv.Profiles.Yield = srv.Ticks.Yield
	srv.Init()
	// Profiler files of an earlier bridge run (profiles do not survive a restart).
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		srv.CleanupAll(ctx, docker)
	}()

	hs := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      150 * time.Second, // histogram: up to 90 s heap walk
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	if cfg.API.SSL.Enabled {
		cr, err := api.NewCertReloader(cfg.API.SSL.Cert, cfg.API.SSL.Key)
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		hs.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: cr.GetCertificate}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", hs.Addr, "tls", cfg.API.SSL.Enabled, "version", Version)
		if cfg.API.SSL.Enabled {
			errc <- hs.ListenAndServeTLS("", "")
		} else {
			errc <- hs.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
