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
	if j.HostPID != 1002 || j.NSPID != 26 || j.User != "container" || filepath.Base(j.Hsperf) != "26" {
		t.Errorf("got %+v", j)
	}
	if filepath.Base(j.AttachSocket()) != ".java_pid26" {
		t.Errorf("socket %s", j.AttachSocket())
	}

	// Without hsperfdata anywhere the first java process (BFS) wins.
	os.RemoveAll(filepath.Join(root, "1002", "root", "tmp", "hsperfdata_container"))
	j, err = p.FindJava(1000)
	if err != nil || j.HostPID != 1003 || j.Hsperf != "" || j.User != "" {
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
	if err != nil || j.Hsperf != "" {
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
