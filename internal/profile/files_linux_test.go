package profile

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
)

// TestTmpSymlink: a container whose /tmp points into the host gets no
// library there, and no host file is removed or read.
func TestTmpSymlink(t *testing.T) {
	m, tg := newManager(t)
	host := t.TempDir()
	os.WriteFile(filepath.Join(host, "mpa-a.log"), []byte("host"), 0o644)
	root := t.TempDir()
	if err := os.Symlink(host, filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	tg.Tmp = ctmp.Tmp(root)
	if err := m.ensureLib(tg); err == nil {
		t.Error("library written")
	}
	if _, err := os.Lstat(filepath.Join(host, LibName)); err == nil {
		t.Error("library created on the host")
	}
	m.Cleanup(context.Background(), tg, false)
	m.removeFiles(tg, "a")
	if _, err := os.Lstat(filepath.Join(host, "mpa-a.log")); err != nil {
		t.Error("host file removed")
	}
	if b, _ := readFile(tg.Tmp, "mpa-a.log", 100); b != nil {
		t.Errorf("host file read: %q", b)
	}
	if _, ok := freeBytes(tg.Tmp); ok {
		t.Error("statfs of the host")
	}
}

// TestEnsureLibReplaces: only a regular file of root with mode 0644 and our
// content is kept, decided on the opened file.
func TestEnsureLibReplaces(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root, like the bridge")
	}
	m, tg := newManager(t)
	lib := filepath.Join(tg.Tmp.Root, LibName)
	check := func(what string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- m.ensureLib(tg) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: blocked", what)
		}
		st, err := os.Lstat(lib)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o644 || st.Sys().(*syscall.Stat_t).Uid != 0 {
			t.Errorf("%s: %v %v", what, st, err)
		}
		if b, _ := os.ReadFile(lib); string(b) != "library v1" {
			t.Errorf("%s: content %q", what, b)
		}
	}
	os.WriteFile(lib, []byte("library v1"), 0o644)
	os.Chown(lib, 1000, 1000)
	check("owned by the container user")
	os.Chmod(lib, 0o666)
	check("writable by others")
	os.Remove(lib)
	syscall.Mkfifo(lib, 0o644)
	check("FIFO")
}
