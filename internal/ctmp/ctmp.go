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
//
// The bridge looks up the JVM's PID once per request; the JVM can end in
// between and its PID go to another process, e.g. one of the host, whose
// root is the host's. A Dir bound to the process (Proc, StartTime) reaches
// the root through a descriptor of /proc/<pid>: the start time is checked on
// it, and once that process has ended nothing below the descriptor resolves
// any more, also not for a new process with the same PID.
package ctmp

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// Errors besides those of the system.
var (
	ErrBadName    = errors.New("ctmp: not a plain name")
	ErrNotRegular = errors.New("ctmp: not a regular file")
	ErrNotSocket  = errors.New("ctmp: not a socket")
	// ErrGone: the process a Dir is bound to has ended; its PID may belong
	// to another process by now.
	ErrGone = errors.New("ctmp: the process has ended")
)

// Dir is a directory inside a container.
type Dir struct {
	Root string // the container's root on the host: /proc/<host-pid>/root (a plain directory in tests)
	Path string // below Root: plain names separated by "/"; "" is Root itself
	// Proc binds d to one process: its /proc/<host-pid> directory, of which
	// Root is "root", and its start time (ParseStartTime). "": Root is
	// opened by its path.
	Proc      string
	StartTime uint64
}

// Tmp is the /tmp of the container whose root is root.
func Tmp(root string) Dir { return Dir{Root: root, Path: "tmp"} }

// ProcTmp is the /tmp of the container of the process whose /proc directory
// is proc and that started at start; the Dir is bound to that process.
func ProcTmp(proc string, start uint64) Dir {
	return Dir{Root: filepath.Join(proc, "root"), Path: "tmp", Proc: proc, StartTime: start}
}

// Sub is the subdirectory name of d.
func (d Dir) Sub(name string) Dir {
	if d.Path == "" {
		d.Path = name
	} else {
		d.Path += "/" + name
	}
	return d
}

var errBadStat = errors.New("ctmp: malformed stat")

// ParseStartTime returns the start time of a process from its
// /proc/<pid>/stat: field 22, clock ticks after boot. PID and start time
// together name one process, a PID alone is reused. The comm field (2) may
// contain spaces and parens, so the fields are counted after its last ')'.
func ParseStartTime(stat []byte) (uint64, error) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, errBadStat
	}
	f := strings.Fields(string(stat[end+1:])) // from field 3
	if len(f) < 20 {
		return 0, errBadStat
	}
	return strconv.ParseUint(f[19], 10, 64)
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
