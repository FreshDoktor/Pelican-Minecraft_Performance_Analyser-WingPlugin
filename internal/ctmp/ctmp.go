// Package ctmp is the only way the bridge touches files inside a container
// (hsperfdata, the attach files, the profiler's files in /tmp).
//
// The container's tenant controls everything below its root, and the bridge
// runs as root. The host reaches that tree through /proc/<pid>/root, where
// the kernel resolves an absolute symlink in any component against the
// host's root, not the container's. So the root is opened once per
// operation, every component below it is opened with O_NOFOLLOW relative to
// the one before, and the operation works on the last descriptor; no path
// below the root is ever handed to the kernel as a whole.
package ctmp

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
)

// Errors besides those of the system.
var (
	ErrBadName    = errors.New("ctmp: not a plain name")
	ErrNotRegular = errors.New("ctmp: not a regular file")
	ErrNotSocket  = errors.New("ctmp: not a socket")
)

// Dir is a directory inside a container.
type Dir struct {
	Root string // the container's root on the host: /proc/<host-pid>/root (a plain directory in tests)
	Path string // below Root: plain names separated by "/"; "" is Root itself
}

// Tmp is the /tmp of the container whose root is root.
func Tmp(root string) Dir { return Dir{Root: root, Path: "tmp"} }

// Sub is the subdirectory name of d.
func (d Dir) Sub(name string) Dir {
	if d.Path == "" {
		return Dir{Root: d.Root, Path: name}
	}
	return Dir{Root: d.Root, Path: d.Path + "/" + name}
}

// String is the host path of d, for messages only: the kernel would follow
// symlinks in it.
func (d Dir) String() string { return filepath.Join(d.Root, filepath.FromSlash(d.Path)) }

// File is a file inside a container.
type File struct {
	Dir  Dir
	Name string // a plain name
}

// IsZero reports whether f names no file.
func (f File) IsZero() bool { return f == File{} }

// String is the host path of f, for messages only.
func (f File) String() string { return filepath.Join(f.Dir.String(), f.Name) }

// components splits d.Path into plain names.
func (d Dir) components() ([]string, error) {
	if d.Path == "" {
		return nil, nil
	}
	names := strings.Split(d.Path, "/")
	for _, n := range names {
		if !plain(n) {
			return nil, d.err("walk", n, ErrBadName)
		}
	}
	return names, nil
}

// plain reports whether name is one component that stays in its directory.
func plain(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsRune(name, '/') && !strings.ContainsRune(name, filepath.Separator) && !strings.ContainsRune(name, 0)
}

func (d Dir) err(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: filepath.Join(d.String(), name), Err: err}
}
