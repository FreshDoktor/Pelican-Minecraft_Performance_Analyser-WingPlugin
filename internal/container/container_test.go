package container

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeProc builds a /proc tree: pid → (ppid, comm, nspid).
func fakeProc(t *testing.T, procs map[int][3]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(filepath.Join(dir, "root", "tmp"), 0o755); err != nil {
			t.Fatal(err)
		}
		stat := strconv.Itoa(pid) + " (" + p[1] + ") S " + p[0] + " 1 1 0 -1 4194560 100 0 0 0 1234 567 0 0 20 0 30 0 100 0 0"
		write(t, filepath.Join(dir, "stat"), stat)
		write(t, filepath.Join(dir, "comm"), p[1]+"\n")
		write(t, filepath.Join(dir, "status"), "Name:\t"+p[1]+"\nPid:\t"+strconv.Itoa(pid)+"\nNSpid:\t"+strconv.Itoa(pid)+"\t"+p[2]+"\n")
	}
	return root
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindJava(t *testing.T) {
	root := fakeProc(t, map[int][3]string{
		1000: {"1", "tini", "1"},
		1001: {"1000", "bash", "7"},
		1002: {"1001", "java", "26"}, // the server JVM, has hsperfdata
		1003: {"1000", "java", "30"}, // e.g. a helper JVM with -XX:-UsePerfData, found first by BFS
		1004: {"1", "java", "5"},     // other container
		1005: {"1002", "(sd-pam) x", "40"},
	})
	write(t, filepath.Join(root, "1002", "root", "tmp", "hsperfdata_container", "26"), "x")
	write(t, filepath.Join(root, "1002", "root", "tmp", "hsperfdata_other", "99"), "x")

	p := Proc{Root: root}
	j, err := p.FindJava(1000)
	if err != nil {
		t.Fatal(err)
	}
	if j.HostPID != 1002 || j.NSPID != 26 || j.User != "container" || j.Hsperf.Name != "26" || j.Hsperf.Dir.Path != "tmp/hsperfdata_container" {
		t.Errorf("got %+v", j)
	}
	if j.Tmp.Root != filepath.Join(root, "1002", "root") || j.AttachSocket().Name != ".java_pid26" || j.AttachSocket().Dir != j.Tmp {
		t.Errorf("tmp %+v, socket %+v", j.Tmp, j.AttachSocket())
	}
	// Bound to the process by its start time (stat field 22), also the hsperfdata file.
	if j.StartTime != 100 || j.Tmp.Proc != filepath.Join(root, "1002") || j.Tmp.StartTime != 100 || j.Hsperf.Dir.StartTime != 100 {
		t.Errorf("start time %d, tmp %+v, hsperf %+v", j.StartTime, j.Tmp, j.Hsperf.Dir)
	}
	if f, err := j.Hsperf.Dir.Open(j.Hsperf.Name); err != nil {
		t.Errorf("hsperfdata: %v", err)
	} else {
		f.Close()
	}

	// Without hsperfdata anywhere the first java process (BFS) wins.
	os.RemoveAll(filepath.Join(root, "1002", "root", "tmp", "hsperfdata_container"))
	j, err = p.FindJava(1000)
	if err != nil || j.HostPID != 1003 || !j.Hsperf.IsZero() || j.User != "" {
		t.Errorf("fallback: %+v %v", j, err)
	}

	if _, err := p.FindJava(1001 + 100); !errors.Is(err, ErrNoJava) {
		t.Errorf("unknown pid: %v", err)
	}
	ticks, err := p.CPUTicks(1002)
	if err != nil || ticks != 1234+567 {
		t.Errorf("cpu ticks %d %v", ticks, err)
	}
}

func TestFindJavaIgnoresSymlinks(t *testing.T) {
	root := fakeProc(t, map[int][3]string{2000: {"1", "java", "26"}})
	target := t.TempDir()
	write(t, filepath.Join(target, "26"), "host file")
	link := filepath.Join(root, "2000", "root", "tmp", "hsperfdata_evil")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks not available:", err)
	}
	j, err := Proc{Root: root}.FindJava(2000)
	if err != nil || !j.Hsperf.IsZero() {
		t.Errorf("symlinked hsperfdata dir must be ignored: %+v %v", j, err)
	}
}

func TestStatFields(t *testing.T) {
	f := statFields("42 (Server (thread) x) S 7 1 1")
	if len(f) != 6 || f[1] != "Server (thread) x" || f[3] != "7" {
		t.Errorf("%q", f)
	}
}

const sampleInspect = `{
  "Id": "f00",
  "Name": "/1a7ce997-259b-452e-8b4e-cecc464142ca",
  "State": {"Status": "running", "Running": true, "Paused": false, "Pid": 2817301, "StartedAt": "2026-10-24T18:29:53.104512Z"},
  "Config": {"Labels": {"Service": "Pelican", "ContainerType": "server_process"}},
  "HostConfig": {"Memory": 2299579597, "MemoryReservation": 1999634432, "NanoCpus": 0, "CpuQuota": 100000, "CpuPeriod": 100000, "CpusetCpus": ""}
}`

func TestParseInspect(t *testing.T) {
	const uuid = "1a7ce997-259b-452e-8b4e-cecc464142ca"
	info, err := parseInspect(uuid, []byte(sampleInspect))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Running || info.State != "running" || info.PID != 2817301 || info.Memory != 2299579597 ||
		info.CPUQuota != 100000 || info.StartedAt.UnixMilli() != 1792866593104 {
		t.Errorf("%+v", info)
	}

	paused := []byte(`{"Name":"/` + uuid + `","State":{"Status":"paused","Running":true,"Pid":5},"Config":{"Labels":{"Service":"Pelican","ContainerType":"server_process"}}}`)
	if info, _ := parseInspect(uuid, paused); info.Running {
		t.Error("paused must not count as running")
	}
	for name, body := range map[string]string{
		"installer": `{"Name":"/` + uuid + `","Config":{"Labels":{"Service":"Pelican","ContainerType":"server_installer"}}}`,
		"foreign":   `{"Name":"/` + uuid + `","Config":{"Labels":{}}}`,
		"id prefix": `{"Name":"/something-else","Config":{"Labels":{"Service":"Pelican","ContainerType":"server_process"}}}`,
	} {
		if _, err := parseInspect(uuid, []byte(body)); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestThreadID(t *testing.T) {
	root := t.TempDir()
	task := func(pid, tid int, comm string) {
		write(t, filepath.Join(root, strconv.Itoa(pid), "task", strconv.Itoa(tid), "comm"), comm+"\n")
	}
	task(26, 26, "java")
	task(26, 27, "java")
	task(26, 41, "C2 CompilerThre")
	task(26, 61, "Server thread")
	task(26, 60, "Server thread")
	task(26, 70, "Server Watchdog")
	p := Proc{Root: root}
	if tid, named, err := p.ThreadID(26, "Server thread"); tid != 60 || !named || err != nil {
		t.Errorf("Server thread: %d %v %v", tid, named, err)
	}
	// Names are cut to 15 bytes, like comm.
	if tid, _, err := p.ThreadID(26, "C2 CompilerThread0"); tid != 41 || err != nil {
		t.Errorf("long name: %d %v", tid, err)
	}
	// A proxy: names, but no Server thread.
	if _, named, err := p.ThreadID(26, "Netty Epoll Server IO #0"); !named || !errors.Is(err, ErrNoThread) {
		t.Errorf("missing: %v %v", named, err)
	}
	// Java 8 names no threads.
	task(30, 30, "java")
	task(30, 31, "java")
	if _, named, err := p.ThreadID(30, "Server thread"); named || !errors.Is(err, ErrNoThread) {
		t.Errorf("java 8: %v %v", named, err)
	}
	if _, _, err := p.ThreadID(99, "Server thread"); err == nil {
		t.Error("no process: no error")
	}
	// In a container the JVM knows the id of its PID namespace (NSpid).
	task(4100, 4100, "java")
	task(4100, 4134, "Server thread")
	write(t, filepath.Join(root, "4100", "task", "4134", "status"), "Name:\tServer thread\nPid:\t4134\nNSpid:\t4134\t60\n")
	if tid, _, err := p.ThreadID(4100, "Server thread"); tid != 60 || err != nil {
		t.Errorf("namespace: %d %v", tid, err)
	}
	if tid, err := p.HostTID(4100, 60); tid != 4134 || err != nil {
		t.Errorf("host id: %d %v", tid, err)
	}
	// Without NSpid (no namespace) both ids are the same.
	if tid, err := p.HostTID(26, 60); tid != 60 || err != nil {
		t.Errorf("host id without NSpid: %d %v", tid, err)
	}
	if _, err := p.HostTID(4100, 61); !errors.Is(err, ErrNoThread) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestSchedStat(t *testing.T) {
	root := t.TempDir()
	// From the test node, 2026-10-26.
	write(t, filepath.Join(root, "1442433", "task", "1442467", "schedstat"), "2364390906440 196701662398 28187259\n")
	p := Proc{Root: root}
	if run, wait, err := p.SchedStat(1442433, 1442467); run != 2364390906440 || wait != 196701662398 || err != nil {
		t.Errorf("schedstat: %d %d %v", run, wait, err)
	}
	if _, _, err := p.SchedStat(1442433, 1); err == nil {
		t.Error("no thread: no error")
	}
	write(t, filepath.Join(root, "1442433", "task", "2", "schedstat"), "12\n")
	if _, _, err := p.SchedStat(1442433, 2); err == nil {
		t.Error("short: no error")
	}
}
