package attach

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"syscall"
)

// oPath is O_PATH (010000000 on all Linux architectures Go supports but
// alpha/parisc/sparc); package syscall does not define it.
const oPath = 0x200000

func sigquit(pid int) error { return syscall.Kill(pid, syscall.SIGQUIT) }

// dialSocket connects without following symlinks: the container controls the
// path, and an absolute symlink would resolve against the host root (e.g. the
// Docker socket). The socket is opened with O_PATH|O_NOFOLLOW, checked, and
// connected through /proc/self/fd/<n>, so no swap between check and connect
// is possible.
func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	fd, err := syscall.Open(path, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		return nil, fmt.Errorf("%s is not a socket", path)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", "/proc/self/fd/"+strconv.Itoa(fd))
}
