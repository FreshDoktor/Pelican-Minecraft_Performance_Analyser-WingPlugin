//go:build !linux

package ctmp

// The bridge runs on Linux only. Elsewhere (tests on Windows) the files are
// reached by path, which follows symlinks; only the names are checked, a
// bound Dir's Proc is ignored.

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
)

func (d Dir) dir() (string, error) {
	names, err := d.components()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{d.Root}, names...)...), nil
}

func (d Dir) path(op, name string) (string, error) {
	if !plain(name) {
		return "", d.err(op, name, ErrBadName)
	}
	p, err := d.dir()
	return filepath.Join(p, name), err
}

func (d Dir) Open(name string) (*os.File, error) {
	p, err := d.path("open", name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, d.err("open", name, ErrNotRegular)
	}
	return f, nil
}

func (d Dir) Create(name string, perm fs.FileMode) (*os.File, error) {
	p, err := d.path("create", name)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err == nil {
		f.Chmod(perm)
	}
	return f, err
}

func (d Dir) Remove(name string) error {
	p, err := d.path("unlinkat", name)
	if err != nil {
		return err
	}
	if st, err := os.Lstat(p); err != nil {
		return err
	} else if st.IsDir() {
		return d.err("unlinkat", name, errors.New("is a directory"))
	}
	return os.Remove(p)
}

func (d Dir) Mode(name string) (fs.FileMode, error) {
	p, err := d.path("fstatat", name)
	if err != nil {
		return 0, err
	}
	st, err := os.Lstat(p)
	if err != nil {
		return 0, err
	}
	return st.Mode(), nil
}

func (d Dir) Names() ([]string, error) {
	p, err := d.dir()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	slices.Sort(names)
	return names, err
}

func (d Dir) Free() (int64, error) { return 0, errors.New("ctmp: free space is only known on Linux") }

func (d Dir) Dial(ctx context.Context, name string) (net.Conn, error) {
	p, err := d.path("dial", name)
	if err != nil {
		return nil, err
	}
	var dl net.Dialer
	return dl.DialContext(ctx, "unix", p)
}
