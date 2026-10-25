//go:build !linux

package attach

import (
	"context"
	"errors"
	"net"
)

func sigquit(int) error { return errors.New("SIGQUIT is only supported on Linux") }

func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
