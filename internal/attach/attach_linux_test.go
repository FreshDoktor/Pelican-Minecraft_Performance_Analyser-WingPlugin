package attach

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

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
	c.Signal = func(int, uint64) error { signalled = true; return nil }
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

func procStart(t *testing.T, pid int) uint64 {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	st, err := ctmp.ParseStartTime(b)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestSigquitStartTime: a PID that names a process of another start time
// (the JVM ended, the PID was reused) gets no signal and no trigger file;
// the process of the right start time gets SIGQUIT.
func TestSigquitStartTime(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep:", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	pid := cmd.Process.Pid
	st := procStart(t, pid)

	if err := sigquit(pid, st+1); !errors.Is(err, ctmp.ErrGone) {
		t.Errorf("sigquit with another start time: %v", err)
	}
	// The whole start: no file operation, no signal.
	proc := "/proc/" + strconv.Itoa(pid)
	tg := Target{HostPID: pid, NSPID: 4000000 + os.Getpid()%100000, Tmp: ctmp.ProcTmp(proc, st+1), StartTime: st + 1}
	c := New(1 << 20)
	signalled := false
	c.Signal = func(int, uint64) error { signalled = true; return nil }
	if err := c.Start(context.Background(), tg); !errors.Is(err, ctmp.ErrGone) || signalled {
		t.Errorf("start: %v, signalled %v", err, signalled)
	}
	if _, err := os.Lstat(filepath.Join("/tmp", tg.trigger())); err == nil {
		os.Remove(filepath.Join("/tmp", tg.trigger()))
		t.Error("trigger file created")
	}
	time.Sleep(50 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the process got a signal: %v", err)
	}

	if err := sigquit(pid, st); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGQUIT {
		t.Errorf("wait: %v", err)
	}
	// Ended and reaped: gone, no signal to whoever gets the PID next.
	if err := sigquit(pid, st); !errors.Is(err, ctmp.ErrGone) {
		t.Errorf("sigquit after the end: %v", err)
	}
}
