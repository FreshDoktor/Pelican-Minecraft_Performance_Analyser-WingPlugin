//go:build !linux

package attach

import "errors"

func sigquit(int, uint64) error { return errors.New("SIGQUIT is only supported on Linux") }
