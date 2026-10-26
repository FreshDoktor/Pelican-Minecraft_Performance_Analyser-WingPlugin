package ctmp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newHost is a host directory the container must not reach, with a file
// hsperfdata_x/26 and a listening socket .java_pid26 (a host JVM).
func newHost(t *testing.T) string {
	t.Helper()
	host := t.TempDir()
	os.WriteFile(filepath.Join(host, "26"), []byte("host"), 0o644)
	os.WriteFile(filepath.Join(host, "mpa-1.log"), []byte("host"), 0o644)
	os.Mkdir(filepath.Join(host, "hsperfdata_x"), 0o755)
	os.WriteFile(filepath.Join(host, "hsperfdata_x", "26"), []byte("host"), 0o644)
	l, err := net.Listen("unix", filepath.Join(host, ".java_pid26"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Error("host socket reached")
			c.Close()
		}
	}()
	return host
}

// refused checks that no operation in d reaches the host directory.
func refused(t *testing.T, what string, d Dir, host string) {
	t.Helper()
	if f, err := d.Open("26"); err == nil {
		b, _ := io.ReadAll(f)
		f.Close()
		t.Errorf("%s: open read %q", what, b)
	}
	if f, err := d.Create("mpa-libasyncProfiler.so", 0o644); err == nil {
		f.Close()
		t.Errorf("%s: created", what)
	}
	if err := d.Remove("mpa-1.log"); err == nil {
		t.Errorf("%s: removed", what)
	}
	if _, err := d.Mode("26"); err == nil {
		t.Errorf("%s: mode", what)
	}
	if _, err := d.Names(); err == nil {
		t.Errorf("%s: listed", what)
	}
	if _, err := d.Free(); err == nil {
		t.Errorf("%s: statfs", what)
	}
	if c, err := d.Dial(context.Background(), ".java_pid26"); err == nil {
		c.Close()
		t.Errorf("%s: dialled", what)
	}
	if _, err := d.Sub("hsperfdata_x").Open("26"); err == nil {
		t.Errorf("%s: open below", what)
	}
	if _, err := os.Lstat(filepath.Join(host, "mpa-libasyncProfiler.so")); err == nil {
		t.Errorf("%s: file created on the host", what)
	}
	if _, err := os.Lstat(filepath.Join(host, "mpa-1.log")); err != nil {
		t.Errorf("%s: host file removed", what)
	}
}

func TestTmpSymlink(t *testing.T) {
	host := newHost(t)
	root := t.TempDir()
	// Absolute: through /proc/<pid>/root it would resolve against the host's root.
	os.Symlink(host, filepath.Join(root, "tmp"))
	refused(t, "absolute tmp", Tmp(root), host)

	// Relative, even within the container: no symlink is followed at all.
	root = t.TempDir()
	os.Mkdir(filepath.Join(root, "real"), 0o755)
	os.WriteFile(filepath.Join(root, "real", "26"), []byte("x"), 0o644)
	os.Symlink("real", filepath.Join(root, "tmp"))
	if _, err := Tmp(root).Open("26"); err == nil {
		t.Error("relative tmp symlink followed")
	}
	os.Remove(filepath.Join(root, "tmp"))
	rel, _ := filepath.Rel(root, host)
	os.Symlink(rel, filepath.Join(root, "tmp"))
	refused(t, "relative tmp to the host", Tmp(root), host)
}

func TestHsperfdataSymlink(t *testing.T) {
	host := newHost(t)
	_, tmp := newRoot(t)
	dir := filepath.Join(tmp.String(), "hsperfdata_x")
	os.Symlink(host, dir)
	refused(t, "hsperfdata symlink", tmp.Sub("hsperfdata_x"), host)

	// Swapped after the lookup: the next read must not follow it.
	os.Remove(dir)
	os.Mkdir(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "26"), []byte("container"), 0o644)
	f := File{Dir: tmp.Sub("hsperfdata_x"), Name: "26"}
	if r, err := f.Dir.Open(f.Name); err != nil {
		t.Fatal(err)
	} else {
		r.Close()
	}
	os.Rename(dir, dir+".old")
	os.Symlink(host, dir)
	if r, err := f.Dir.Open(f.Name); err == nil {
		b, _ := io.ReadAll(r)
		r.Close()
		t.Errorf("swapped directory followed: %q", b)
	}
}

func TestFinalSymlink(t *testing.T) {
	host := newHost(t)
	_, tmp := newRoot(t)
	os.Symlink(filepath.Join(host, "26"), filepath.Join(tmp.String(), "26"))
	os.Symlink(filepath.Join(host, ".java_pid26"), filepath.Join(tmp.String(), ".java_pid26"))
	os.Symlink(filepath.Join(host, "mpa-1.log"), filepath.Join(tmp.String(), "mpa-1.log"))
	if _, err := tmp.Open("26"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("open: %v", err)
	}
	if _, err := tmp.Dial(context.Background(), ".java_pid26"); !errors.Is(err, ErrNotSocket) {
		t.Errorf("dial: %v", err)
	}
	if _, err := tmp.Create("26", 0o644); !errors.Is(err, fs.ErrExist) {
		t.Errorf("create: %v", err)
	}
	if m, err := tmp.Mode("26"); err != nil || m&fs.ModeSymlink == 0 {
		t.Errorf("mode %v %v", m, err)
	}
	// Remove takes the link, not its target.
	if err := tmp.Remove("mpa-1.log"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(host, "mpa-1.log")); string(b) != "host" {
		t.Error("symlink target removed")
	}
}

func TestFIFO(t *testing.T) {
	_, tmp := newRoot(t)
	if err := syscall.Mkfifo(filepath.Join(tmp.String(), "26"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := tmp.Open("26")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("fifo: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("open blocked on a FIFO")
	}
	if m, err := tmp.Mode("26"); err != nil || m&fs.ModeNamedPipe == 0 {
		t.Errorf("mode %v %v", m, err)
	}
	if _, err := tmp.Dial(context.Background(), "26"); !errors.Is(err, ErrNotSocket) {
		t.Errorf("dial: %v", err)
	}
}

func TestCreateMode(t *testing.T) {
	_, tmp := newRoot(t)
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	f, err := tmp.Create("mpa-libasyncProfiler.so", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	f.Close()
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v despite the umask", st.Mode())
	}
}

func TestFree(t *testing.T) {
	_, tmp := newRoot(t)
	if n, err := tmp.Free(); err != nil || n <= 0 {
		t.Errorf("free %d %v", n, err)
	}
}

func TestProcRoot(t *testing.T) {
	// The real thing: the root of a process through /proc, here our own.
	names, err := Dir{Root: "/proc/self/root", Path: "proc"}.Names()
	if err != nil || len(names) == 0 {
		t.Fatalf("names %v %v", names, err)
	}
	// /proc/self is a symlink: refused below the root, too.
	if _, err := (Dir{Root: "/proc/self/root", Path: "proc/self"}).Names(); err == nil {
		t.Error("followed the /proc/self symlink")
	}
}

// TestNamespace goes through /proc/<pid>/root of a process in its own mount
// namespace with a tmpfs /tmp, like a container: the absolute symlink
// /tmp/evil -> /etc resolves against the host's root when followed.
func TestNamespace(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	cmd := exec.Command("unshare", "--mount", "--pid", "--fork", "--propagation", "private", "sh", "-c",
		"mount -t tmpfs tmpfs /tmp && mkdir /tmp/hsperfdata_x && echo container > /tmp/hsperfdata_x/26 && "+
			"ln -s /etc /tmp/evil && ln -s /etc /tmp/hsperfdata_y && exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var root string
	for i := 0; i < 200 && root == ""; i++ {
		time.Sleep(10 * time.Millisecond)
		root = childRoot(cmd.Process.Pid)
	}
	if root == "" {
		t.Fatal("no child of unshare")
	}
	tmp := Tmp(root)
	var f *os.File
	var err error
	for i := 0; i < 200; i++ {
		if f, err = tmp.Sub("hsperfdata_x").Open("26"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "container\n" {
		t.Errorf("read %q", b)
	}
	// What the kernel does with a path: the host's /etc.
	if _, err := os.Stat(filepath.Join(root, "tmp", "evil", "passwd")); err != nil {
		t.Skip("symlink not followed to the host here:", err)
	}
	if _, err := tmp.Sub("evil").Names(); err == nil {
		t.Error("listed the host's /etc")
	}
	if f, err := tmp.Sub("hsperfdata_y").Open("passwd"); err == nil {
		f.Close()
		t.Error("opened the host's /etc/passwd")
	}
	if names, err := tmp.Names(); err != nil || len(names) != 3 {
		t.Errorf("names %v %v", names, err)
	}

	// A socket in the container's /tmp.
	l, err := net.Listen("unix", filepath.Join(root, "tmp", ".java_pid26"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			io.WriteString(c, "0\n")
			c.Close()
		}
	}()
	c, err := tmp.Dial(context.Background(), ".java_pid26")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(c)
	c.Close()
	if string(b) != "0\n" {
		t.Errorf("socket read %q", b)
	}
}

// childRoot is /proc/<pid>/root of the first child of ppid.
func childRoot(ppid int) string {
	names, _ := filepath.Glob("/proc/[0-9]*/status")
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "PPid:"); ok && strings.TrimSpace(v) == strconv.Itoa(ppid) {
				return filepath.Join(filepath.Dir(n), "root")
			}
		}
	}
	return ""
}
