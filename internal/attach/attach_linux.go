package attach

import (
	"errors"
	"io/fs"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin/internal/ctmp"
)

// sigquit sends SIGQUIT to the JVM pid that started at start. The JVM may
// have ended since it was found and its PID belong to another process, e.g.
// one of the host, which SIGQUIT terminates with a core dump. A pidfd names
// one process: it is opened first, then pid's start time is checked, so a
// match means the pidfd is that JVM, and the signal reaches it or nobody.
// Without pidfds (Linux before 5.3, or a seccomp filter: ENOSYS, EPERM) the
// check is followed by kill, which leaves a short window.
func sigquit(pid int, start uint64) error {
	fd, err := unix.PidfdOpen(pid, 0)
	switch {
	case err == nil:
		defer unix.Close(fd)
		if err := sameProcess(pid, start); err != nil {
			return err
		}
		return unix.PidfdSendSignal(fd, unix.SIGQUIT, nil, 0)
	case errors.Is(err, unix.ESRCH):
		return ctmp.ErrGone
	case errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM):
		if err := sameProcess(pid, start); err != nil {
			return err
		}
		return unix.Kill(pid, unix.SIGQUIT)
	}
	return err
}

// sameProcess checks that pid is still the process that started at start.
func sameProcess(pid int, start uint64) error {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return ctmp.ErrGone
	}
	if err != nil {
		return err
	}
	st, err := ctmp.ParseStartTime(b)
	if err != nil {
		return err
	}
	if st != start {
		return ctmp.ErrGone
	}
	return nil
}
