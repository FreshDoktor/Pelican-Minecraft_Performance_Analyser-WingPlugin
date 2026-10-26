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
	showPin := flag.Bool("fingerprint", false, "print the pin of the certificate the bridge serves, for the panel, and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("mpa-bridge", Version)
		return
	}
	if *showPin {
		if err := fingerprint(*cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "mpa-bridge:", err)
			if errors.Is(err, errPlainHTTP) {
				os.Exit(3) // install.sh tells plain HTTP from a failure by it
			}
			os.Exit(1)
		}
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
	cr, source, err := certificate(cfg, log)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	if cr == nil && !isLoopback(cfg.Host) {
		log.Warn("tls.disable is set and host is not a loopback address: the token and all data cross the network in cleartext, " +
			"only do this behind a proxy on this node that serves HTTPS")
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
		TLS:        cr != nil,
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
	if cr != nil {
		hs.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: cr.GetCertificate}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		attrs := []any{"addr", hs.Addr, "tls", cr != nil, "version", Version}
		if cr != nil {
			pin, _ := cr.Pin()
			attrs = append(attrs, "cert", source, "pin", pin)
		}
		log.Info("listening", attrs...)
		if cr != nil {
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

// certificate returns the reloader of the certificate the bridge serves and
// its source (see config.CertFiles), nil with tls.disable. The self-signed
// certificate is created here on the first start.
func certificate(cfg *config.Config, log *slog.Logger) (*api.CertReloader, string, error) {
	source, certFile, keyFile, ok := cfg.CertFiles()
	if !ok {
		return nil, "", nil
	}
	if source == config.CertSelfSigned {
		created, err := api.EnsureSelfSigned(certFile, keyFile)
		if err != nil {
			return nil, "", err
		}
		if created && log != nil {
			log.Info("created a self-signed certificate, enter its pin in the panel (mpa-bridge -fingerprint)", "cert", certFile, "key", keyFile)
		}
	}
	cr, err := api.NewCertReloader(certFile, keyFile)
	if err != nil {
		return nil, "", fmt.Errorf("%s certificate: %w", source, err)
	}
	return cr, source, nil
}

// errPlainHTTP is the error of -fingerprint with tls.disable (exit status 3).
var errPlainHTTP = errors.New("there is no certificate to pin")

// fingerprint prints the pin of the certificate the bridge serves (or will
// serve, the self-signed one is created if need be), which the panel checks.
func fingerprint(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	cr, source, err := certificate(cfg, nil)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	if cr == nil {
		return fmt.Errorf("tls.disable is set in %s, the bridge serves plain HTTP: %w", cfgPath, errPlainHTTP)
	}
	pin, err := cr.Pin()
	if err != nil {
		return fmt.Errorf("%s certificate %s: %w", source, cr.CertFile, err)
	}
	fmt.Println(pin)
	return nil
}

// isLoopback reports whether host (of mpa-bridge.yml) only listens on this machine.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
