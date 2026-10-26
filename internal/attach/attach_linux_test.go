package attach

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
)

// TestTmpSymlink: a container whose /tmp points into the host reaches
// neither a host JVM's socket nor gets a trigger file there.
func TestTmpSymlink(t *testing.T) {
	host := t.TempDir()
	f := listen(t, filepath.Join(host, ".java_pid26"), func([]string) string { return "0\nhost JVM\n" })
	root := t.TempDir()
	if err := os.Symlink(host, filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	tg := Target{HostPID: 4711, NSPID: 26, Tmp: ctmp.Tmp(root)}
	c := New(1 << 20)
	signalled := false
	c.Signal = func(int) error { signalled = true; return nil }
	if SocketExists(tg.Socket()) {
		t.Error("host socket seen")
	}
	if err := c.Start(context.Background(), tg); err == nil || signalled {
		t.Errorf("start: %v, signalled %v", err, signalled)
	}
	if _, err := os.Lstat(filepath.Join(host, ".attach_pid26")); err == nil {
		t.Error("trigger file created on the host")
	}
	if _, err := c.Jcmd(context.Background(), tg, "VM.version"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("jcmd: %v", err)
	}
	if _, err := dialSocket(context.Background(), tg.Socket()); err == nil {
		t.Error("dialled the host socket")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 {
		t.Errorf("host JVM got %q", f.requests)
	}
}

// TestDialSocketSymlink: a socket name that is a symlink is not followed.
func TestDialSocketSymlink(t *testing.T) {
	host := t.TempDir()
	listen(t, filepath.Join(host, "docker.sock"), func([]string) string { return "0\n" })
	dir := t.TempDir()
	os.Symlink(filepath.Join(host, "docker.sock"), filepath.Join(dir, ".java_pid26"))
	tg := Target{HostPID: 4711, NSPID: 26, Tmp: ctmp.Dir{Root: dir}}
	if SocketExists(tg.Socket()) {
		t.Error("symlink counts as socket")
	}
	if _, err := dialSocket(context.Background(), tg.Socket()); !errors.Is(err, ctmp.ErrNotSocket) {
		t.Errorf("dial: %v", err)
	}
}
