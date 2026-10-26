package attach

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/container"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/hsperf"
)

// TestRealJVM attaches to a running JVM on Linux, as root, like on a node:
//
//	unshare --pid --fork --mount-proc java -m jdk.httpserver &   # JVM gets NSpid != host PID
//	MPA_REAL_JVM_INIT=<pid of unshare> MPA_REAL_JVM_OUT=/tmp/out go test ./internal/attach -run RealJVM -v
//
// It finds the JVM below MPA_REAL_JVM_INIT, reads its hsperfdata, starts the
// listener (trigger file + SIGQUIT) if needed and runs the jcmd commands of
// the bridge. Outputs go to MPA_REAL_JVM_OUT if set.
func TestRealJVM(t *testing.T) {
	initPID, _ := strconv.Atoi(os.Getenv("MPA_REAL_JVM_INIT"))
	if initPID == 0 {
		t.Skip("MPA_REAL_JVM_INIT not set")
	}
	j, err := container.Proc{Root: "/proc"}.FindJava(initPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("java host pid %d, ns pid %d, user %q, hsperf %s", j.HostPID, j.NSPID, j.User, j.Hsperf)
	if j.Hsperf.IsZero() {
		t.Fatal("no hsperfdata")
	}
	d, err := hsperf.ReadFile(j.Hsperf)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := d.String("java.property.java.version")
	t.Logf("hsperf: %d entries, java %s, gc %s", d.Len(), v, d.GCType())

	c := New(32 << 20)
	tg := Target{HostPID: j.HostPID, NSPID: j.NSPID, Tmp: j.Tmp, StartTime: j.StartTime}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wasRunning := SocketExists(tg.Socket())
	if err := c.Start(ctx, tg); err != nil {
		t.Fatal("start:", err)
	}
	t.Logf("listener was running: %v; trigger left behind: %v", wasRunning, exists(tg.Tmp, tg.trigger()))
	if exists(tg.Tmp, tg.trigger()) {
		t.Error("trigger file not removed")
	}
	out := os.Getenv("MPA_REAL_JVM_OUT")
	for _, cmd := range []string{"VM.flags", "GC.heap_info", "VM.metaspace", "Thread.print -l", "GC.class_histogram -all", "VM.version"} {
		start := time.Now()
		s, err := c.Jcmd(ctx, tg, cmd)
		if err != nil {
			t.Errorf("%s: %v", cmd, err)
			continue
		}
		t.Logf("%s: %d bytes in %v, first line %q", cmd, len(s), time.Since(start), firstLine(s))
		if out != "" {
			os.MkdirAll(out, 0o755)
			os.WriteFile(filepath.Join(out, strings.ReplaceAll(cmd, " ", "_")+".txt"), []byte(s), 0o644)
		}
	}
	_, err = c.Jcmd(ctx, tg, "No.such_command")
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Errorf("unknown command: %v", err)
	} else {
		t.Logf("unknown command: code %d, %q", ce.Code, firstLine(ce.Message))
	}
	small := New(100)
	if _, err := small.Jcmd(ctx, tg, "Thread.print -l"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("size limit: %v", err)
	}
}

func exists(d ctmp.Dir, name string) bool { _, err := d.Mode(name); return err == nil }

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
