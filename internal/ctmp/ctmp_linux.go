package ctmp

import (
	"context"
	"io/fs"
	"net"
	"os"
	"slices"
	"strconv"

	"golang.org/x/sys/unix"
)

// open returns an O_PATH descriptor of d. Only Root is resolved as a path:
// following the magic link /proc/<pid>/root into the container is the point.
// Each component below it is opened relative to its parent and must be a
// directory; O_NOFOLLOW refuses a symlink (O_DIRECTORY on a symlink fails).
// This works on every kernel, also without openat2 (5.4 on Ubuntu 20.04).
func (d Dir) open() (int, error) {
	names, err := d.components()
	if err != nil {
		return -1, err
	}
	fd, err := openat(unix.AT_FDCWD, d.Root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: d.Root, Err: err}
	}
	walked := d.Root
	for _, n := range names {
		next, err := openat(fd, n, unix.O_PATH|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		walked += "/" + n
		if err != nil {
			return -1, &fs.PathError{Op: "openat", Path: walked, Err: err}
		}
		fd = next
	}
	return fd, nil
}

// at runs fn with the descriptor of d after checking name.
func (d Dir) at(op, name string, fn func(dirfd int) error) error {
	if !plain(name) {
		return d.err(op, name, ErrBadName)
	}
	dirfd, err := d.open()
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	return fn(dirfd)
}

// Open opens a regular file for reading. The name is opened with O_PATH
// first, which never reaches a device driver or blocks on a FIFO (the tenant
// may mknod), and only a regular file is reopened for reading, through
// /proc/self/fd, so it is the same inode.
func (d Dir) Open(name string) (*os.File, error) {
	var f *os.File
	err := d.at("open", name, func(dirfd int) error {
		fd, err := openat(dirfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return d.err("openat", name, err)
		}
		defer unix.Close(fd)
		if err := isType(fd, unix.S_IFREG); err != nil {
			return d.err("open", name, err)
		}
		// The magic link names fd's inode itself, no path is resolved again.
		rfd, err := openat(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(fd),
			unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err != nil {
			return d.err("reopen", name, err)
		}
		f = os.NewFile(uintptr(rfd), d.String()+"/"+name)
		return nil
	})
	return f, err
}

// Create creates name exclusively for writing (O_EXCL never follows a
// planted symlink) and sets perm on the descriptor, regardless of the umask.
func (d Dir) Create(name string, perm fs.FileMode) (*os.File, error) {
	var f *os.File
	err := d.at("create", name, func(dirfd int) error {
		fd, err := openat(dirfd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_NOCTTY|unix.O_CLOEXEC, uint32(perm.Perm()))
		if err != nil {
			return d.err("openat", name, err)
		}
		if err := unix.Fchmod(fd, uint32(perm.Perm())); err != nil {
			unix.Close(fd)
			unix.Unlinkat(dirfd, name, 0)
			return d.err("fchmod", name, err)
		}
		f = os.NewFile(uintptr(fd), d.String()+"/"+name)
		return nil
	})
	return f, err
}

// Remove removes name, never a directory; a symlink itself is removed.
func (d Dir) Remove(name string) error {
	return d.at("unlinkat", name, func(dirfd int) error {
		if err := unix.Unlinkat(dirfd, name, 0); err != nil {
			return d.err("unlinkat", name, err)
		}
		return nil
	})
}

// Mode returns the type and permissions of name, not following a symlink.
func (d Dir) Mode(name string) (fs.FileMode, error) {
	var m fs.FileMode
	err := d.at("fstatat", name, func(dirfd int) error {
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return d.err("fstatat", name, err)
		}
		m = fileMode(st.Mode)
		return nil
	})
	return m, err
}

// Names lists d, sorted. Only names: an entry's type would be looked up by
// path where the filesystem has no d_type.
func (d Dir) Names() ([]string, error) {
	dirfd, err := d.open()
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirfd)
	fd, err := openat(dirfd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, d.err("openat", ".", err)
	}
	f := os.NewFile(uintptr(fd), d.String())
	defer f.Close()
	names, err := f.Readdirnames(-1)
	slices.Sort(names)
	return names, err
}

// Free returns the bytes of d's filesystem available to unprivileged users.
func (d Dir) Free() (int64, error) {
	dirfd, err := d.open()
	if err != nil {
		return 0, err
	}
	defer unix.Close(dirfd)
	var st unix.Statfs_t
	if err := unix.Fstatfs(dirfd, &st); err != nil {
		return 0, d.err("fstatfs", "", err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// Dial connects to the Unix socket name. The socket is opened with
// O_PATH|O_NOFOLLOW, checked, and connected through /proc/self/fd/<n>, so no
// swap between check and connect is possible.
func (d Dir) Dial(ctx context.Context, name string) (net.Conn, error) {
	var c net.Conn
	err := d.at("dial", name, func(dirfd int) error {
		fd, err := openat(dirfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return d.err("openat", name, err)
		}
		defer unix.Close(fd)
		if err := isType(fd, unix.S_IFSOCK); err != nil {
			return d.err("dial", name, err)
		}
		var dl net.Dialer
		c, err = dl.DialContext(ctx, "unix", "/proc/self/fd/"+strconv.Itoa(fd))
		return err
	})
	return c, err
}

// isType checks the file type of fd (S_IFREG, S_IFSOCK).
func isType(fd int, typ uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	switch {
	case st.Mode&unix.S_IFMT == typ:
		return nil
	case typ == unix.S_IFSOCK:
		return ErrNotSocket
	}
	return ErrNotRegular
}

func fileMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= fs.ModeDir
	case unix.S_IFLNK:
		mode |= fs.ModeSymlink
	case unix.S_IFSOCK:
		mode |= fs.ModeSocket
	case unix.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case unix.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		mode |= fs.ModeDevice
	}
	return mode
}

// openat retries on EINTR, like package os.
func openat(dirfd int, path string, flags int, perm uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, path, flags, perm)
		if err != unix.EINTR {
			return fd, err
		}
	}
}
