package container

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

// TestDockerOverUnixSocket runs the client against a fake Engine API.
func TestDockerOverUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets not available:", err)
	}
	const uuid = "1a7ce997-259b-452e-8b4e-cecc464142ca"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case uuid:
			w.Write([]byte(sampleInspect))
		case "broken":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	defer srv.Close()

	d := NewDocker(sock)
	ctx := context.Background()
	info, err := d.Inspect(ctx, uuid)
	if err != nil || info.PID != 2817301 {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := d.Inspect(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := d.Inspect(ctx, "broken"); !errors.Is(err, ErrDockerUnavailable) {
		t.Errorf("broken: %v", err)
	}
	if _, err := NewDocker(filepath.Join(t.TempDir(), "none.sock")).Inspect(ctx, uuid); !errors.Is(err, ErrDockerUnavailable) {
		t.Errorf("no socket: %v", err)
	}
}
