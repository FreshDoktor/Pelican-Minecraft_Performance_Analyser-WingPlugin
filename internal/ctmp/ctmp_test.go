package ctmp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newRoot builds a container root with an empty /tmp.
func newRoot(t *testing.T) (root string, tmp Dir) {
	t.Helper()
	root = t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "tmp"), 0o1777); err != nil {
		t.Fatal(err)
	}
	return root, Tmp(root)
}

func TestFiles(t *testing.T) {
	root, tmp := newRoot(t)
	f, err := tmp.Create("mpa-a.log", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(f, "hello")
	f.Close()
	if _, err := tmp.Create("mpa-a.log", 0o644); !errors.Is(err, fs.ErrExist) {
		t.Errorf("second create: %v", err)
	}
	if m, err := tmp.Mode("mpa-a.log"); err != nil || !m.IsRegular() {
		t.Errorf("mode %v %v", m, err)
	}
	f, err = tmp.Open("mpa-a.log")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "hello" {
		t.Errorf("read %q", b)
	}

	os.Mkdir(filepath.Join(root, "tmp", "hsperfdata_container"), 0o755)
	os.WriteFile(filepath.Join(root, "tmp", "hsperfdata_container", "26"), []byte("perf"), 0o644)
	if names, err := tmp.Names(); err != nil || strings.Join(names, ",") != "hsperfdata_container,mpa-a.log" {
		t.Errorf("names %v %v", names, err)
	}
	if f, err := tmp.Sub("hsperfdata_container").Open("26"); err != nil {
		t.Errorf("open in subdirectory: %v", err)
	} else {
		f.Close()
	}
	if _, err := tmp.Open("hsperfdata_container"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("directory opened: %v", err)
	}
	if err := tmp.Remove("hsperfdata_container"); err == nil {
		t.Error("directory removed")
	}

	if err := tmp.Remove("mpa-a.log"); err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Open("mpa-a.log"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("open after remove: %v", err)
	}
	if err := tmp.Remove("mpa-a.log"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("second remove: %v", err)
	}
	if _, err := (Dir{Root: root, Path: "tmp/missing"}).Names(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing directory: %v", err)
	}
	if got := (File{Dir: tmp, Name: "x"}).String(); got != filepath.Join(root, "tmp", "x") {
		t.Errorf("string %s", got)
	}
	if !(File{}).IsZero() || (File{Dir: tmp, Name: "x"}).IsZero() {
		t.Error("IsZero")
	}
}

func TestBadNames(t *testing.T) {
	root, tmp := newRoot(t)
	os.WriteFile(filepath.Join(root, "secret"), []byte("host"), 0o644)
	for _, name := range []string{"", ".", "..", "../secret", "a/b", "/secret", "x\x00y"} {
		if _, err := tmp.Open(name); !errors.Is(err, ErrBadName) {
			t.Errorf("open %q: %v", name, err)
		}
		if _, err := tmp.Create(name, 0o644); !errors.Is(err, ErrBadName) {
			t.Errorf("create %q: %v", name, err)
		}
		if err := tmp.Remove(name); !errors.Is(err, ErrBadName) {
			t.Errorf("remove %q: %v", name, err)
		}
		if _, err := tmp.Mode(name); !errors.Is(err, ErrBadName) {
			t.Errorf("mode %q: %v", name, err)
		}
		if _, err := tmp.Dial(context.Background(), name); !errors.Is(err, ErrBadName) {
			t.Errorf("dial %q: %v", name, err)
		}
	}
	for _, path := range []string{"tmp/..", "../tmp", "/tmp", "tmp//x", "tmp/."} {
		d := Dir{Root: root, Path: path}
		if _, err := d.Open("secret"); !errors.Is(err, ErrBadName) {
			t.Errorf("path %q: %v", path, err)
		}
		if _, err := d.Names(); !errors.Is(err, ErrBadName) {
			t.Errorf("names of %q: %v", path, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "secret")); string(b) != "host" {
		t.Error("secret changed")
	}
}

func TestDial(t *testing.T) {
	_, tmp := newRoot(t)
	l, err := net.Listen("unix", filepath.Join(tmp.String(), ".java_pid26"))
	if err != nil {
		t.Skip("unix sockets not available:", err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			io.WriteString(c, "0\n")
			c.Close()
		}
	}()
	c, err := tmp.Dial(context.Background(), ".java_pid26")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(c)
	c.Close()
	if string(b) != "0\n" {
		t.Errorf("read %q", b)
	}
	if _, err := tmp.Dial(context.Background(), ".java_pid27"); err == nil {
		t.Error("missing socket dialled")
	}
}

func TestParseStartTime(t *testing.T) {
	// comm may hold spaces and parens; field 22 is counted after the last ')'.
	stat := "4242 (java) x) (y) S 1 4242 4242 0 -1 4194560 100 0 0 0 1234 567 0 0 20 0 30 0 987654 0 0\n"
	if st, err := ParseStartTime([]byte(stat)); err != nil || st != 987654 {
		t.Errorf("start %d %v", st, err)
	}
	for _, bad := range []string{"", "4242 java S 1", "4242 (java) S 1 4242 4242"} {
		if _, err := ParseStartTime([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
