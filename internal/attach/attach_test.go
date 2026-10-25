package attach

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJVM is an attach listener speaking protocol 1 like HotSpot.
type fakeJVM struct {
	t        *testing.T
	l        net.Listener
	mu       sync.Mutex
	requests [][]string
	reply    func(args []string) string // "" = never answer
}

func listen(t *testing.T, path string, reply func([]string) string) *fakeJVM {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Skip("unix sockets not available:", err)
	}
	f := &fakeJVM{t: t, l: l, reply: reply}
	go f.serve()
	t.Cleanup(func() { l.Close() })
	return f
}

func (f *fakeJVM) serve() {
	for {
		c, err := f.l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			// Version, operation and three arguments, each NUL-terminated.
			var parts []string
			var cur bytes.Buffer
			b := make([]byte, 1)
			for len(parts) < 5 {
				if _, err := c.Read(b); err != nil {
					return
				}
				if b[0] == 0 {
					parts = append(parts, cur.String())
					cur.Reset()
					continue
				}
				cur.WriteByte(b[0])
			}
			f.mu.Lock()
			f.requests = append(f.requests, parts)
			f.mu.Unlock()
			out := f.reply(parts)
			if out == "" {
				time.Sleep(10 * time.Second)
				return
			}
			io.WriteString(c, out)
		}(c)
	}
}

func testClient(max int64) *Client {
	c := New(max)
	c.Signal = func(int) error { return errors.New("no signal expected") }
	// Windows reports AF_UNIX socket files differently; existence is enough here.
	c.SocketExists = func(p string) bool { _, err := os.Lstat(p); return err == nil }
	return c
}

func TestJcmd(t *testing.T) {
	dir := t.TempDir()
	tg := Target{HostPID: 4711, NSPID: 26, TmpDir: dir}
	f := listen(t, tg.Socket(), func(a []string) string {
		switch a[2] {
		case "Thread.print -l":
			return "0\n2026-10-25 13:09:43\nFull thread dump ...\n"
		case "Big":
			return "0\n" + strings.Repeat("x", 5000)
		case "Hang":
			return ""
		}
		return "-1\njava.lang.IllegalArgumentException: Unknown diagnostic command\n"
	})
	c := testClient(1000)
	ctx := context.Background()

	out, err := c.Jcmd(ctx, tg, "Thread.print -l")
	if err != nil || out != "2026-10-25 13:09:43\nFull thread dump ...\n" {
		t.Fatalf("%q %v", out, err)
	}
	if got := f.requests[0]; strings.Join(got, "|") != "1|jcmd|Thread.print -l||" {
		t.Errorf("request %q", got)
	}

	_, err = c.Jcmd(ctx, tg, "Nope")
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != -1 || !strings.Contains(ce.Message, "Unknown diagnostic command") {
		t.Errorf("command error: %v", err)
	}
	if _, err := c.Jcmd(ctx, tg, "Big"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Jcmd(tctx, tg, "Hang"); !errors.Is(err, ErrTimeout) || time.Since(start) > 2*time.Second {
		t.Errorf("timeout: %v after %v", err, time.Since(start))
	}
	if _, err := c.Execute(ctx, tg, "jcmd", "a\x00b"); !errors.Is(err, ErrProtocol) {
		t.Errorf("NUL: %v", err)
	}
	if _, err := c.Execute(ctx, tg, "load", "a", "b", "c", "d"); !errors.Is(err, ErrProtocol) {
		t.Errorf("4 args: %v", err)
	}
	missing := Target{HostPID: 1, NSPID: 99, TmpDir: dir}
	if _, err := c.Jcmd(ctx, missing, "Thread.print -l"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("no socket: %v", err)
	}
}

func TestStart(t *testing.T) {
	dir := t.TempDir()
	tg := Target{HostPID: 4711, NSPID: 26, TmpDir: dir}
	c := testClient(1 << 20)
	var signalled []int
	var triggerSeen bool
	c.Signal = func(pid int) error {
		signalled = append(signalled, pid)
		// Like HotSpot: the trigger file must exist when the signal arrives.
		_, err := os.Stat(filepath.Join(dir, ".attach_pid26"))
		triggerSeen = err == nil
		go func() {
			time.Sleep(50 * time.Millisecond)
			listen(t, tg.Socket(), func([]string) string { return "0\nok\n" })
		}()
		return nil
	}
	if err := c.Start(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if len(signalled) != 1 || signalled[0] != 4711 || !triggerSeen {
		t.Errorf("signal %v, trigger seen %v", signalled, triggerSeen)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".attach_pid26")); !os.IsNotExist(err) {
		t.Error("trigger file not removed")
	}
	if out, err := c.Jcmd(context.Background(), tg, "VM.version"); err != nil || out != "ok\n" {
		t.Errorf("%q %v", out, err)
	}
	// Running listener: Start does nothing.
	if err := c.Start(context.Background(), tg); err != nil || len(signalled) != 1 {
		t.Errorf("second start: %v, signals %d", err, len(signalled))
	}
}

func TestStartTimeout(t *testing.T) {
	dir := t.TempDir()
	tg := Target{HostPID: 4711, NSPID: 26, TmpDir: dir}
	c := testClient(1 << 20)
	c.StartTimeout = 100 * time.Millisecond
	c.Signal = func(int) error { return nil } // the JVM never reacts
	if err := c.Start(context.Background(), tg); !errors.Is(err, ErrTimeout) {
		t.Errorf("%v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".attach_pid26")); !os.IsNotExist(err) {
		t.Error("trigger file not removed after timeout")
	}
}

func TestStartRejectsPlantedTrigger(t *testing.T) {
	dir := t.TempDir()
	tg := Target{HostPID: 4711, NSPID: 26, TmpDir: dir}
	if err := os.Mkdir(filepath.Join(dir, ".attach_pid26"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := testClient(1 << 20)
	signalled := false
	c.Signal = func(int) error { signalled = true; return nil }
	if err := c.Start(context.Background(), tg); err == nil || signalled {
		t.Errorf("planted directory must stop the start: %v, signalled %v", err, signalled)
	}
}
