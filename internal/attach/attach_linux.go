package attach

import "syscall"

func sigquit(pid int) error { return syscall.Kill(pid, syscall.SIGQUIT) }
