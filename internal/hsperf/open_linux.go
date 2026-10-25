package hsperf

import (
	"os"
	"syscall"
)

// openFile opens path without following a final symlink and without blocking
// on FIFOs: the file lives in a directory the container controls.
func openFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
