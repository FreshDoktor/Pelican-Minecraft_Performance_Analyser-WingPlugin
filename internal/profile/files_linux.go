package profile

import (
	"os"
	"syscall"
)

// openNoFollow opens a file in the container's /tmp without following a
// final symlink and without blocking on FIFOs.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// freeBytes returns the space available to unprivileged users below dir.
// The JVM writes the profile as the container user, so root reserves do not count.
func freeBytes(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}

func ownedByRoot(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == 0
}
