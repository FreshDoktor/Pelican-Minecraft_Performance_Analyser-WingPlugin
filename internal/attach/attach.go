// Package attach implements the client side of the HotSpot attach protocol
// (what jcmd and jattach do), from the host into a container's JVM.
//
// The JVM listens on /tmp/.java_pid<ns-pid>. If the listener is not running,
// it is started by creating /tmp/.attach_pid<ns-pid> and sending SIGQUIT.
// HotSpot accepts root as peer on both (attachListener_posix.cpp).
package attach

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors of the client.
var (
	ErrNotRunning = errors.New("attach: listener not running")
	ErrTimeout    = errors.New("attach: timeout")
	ErrTooLarge   = errors.New("attach: output too large")
	ErrProtocol   = errors.New("attach: protocol error")
)

// CommandError is a non-zero result code of the JVM.
type CommandError struct {
	Code    int
	Message string
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("attach: command failed (code %d): %s", e.Code, e.Message)
}

// Target is a JVM seen from the host.
type Target struct {
	HostPID int    // PID on the host, receives SIGQUIT
	NSPID   int    // PID inside the container, names the files
	TmpDir  string // host path of the container's /tmp (/proc/<host-pid>/root/tmp)
}

// Socket is the host path of the attach socket.
func (t Target) Socket() string { return filepath.Join(t.TmpDir, ".java_pid"+strconv.Itoa(t.NSPID)) }

func (t Target) trigger() string { return filepath.Join(t.TmpDir, ".attach_pid"+strconv.Itoa(t.NSPID)) }

// Client runs commands. The zero value is not usable; use New.
type Client struct {
	// MaxOutput limits the bytes read from the JVM.
	MaxOutput int64
	// StartTimeout is how long to wait for the socket after SIGQUIT (5 s).
	StartTimeout time.Duration

	// Overridable in tests.
	Signal       func(pid int) error // sends SIGQUIT
	Dial         func(ctx context.Context, path string) (net.Conn, error)
	SocketExists func(path string) bool

	mu    sync.Mutex
	locks map[int]chan struct{}
}

// New returns a client with the production defaults.
func New(maxOutput int64) *Client {
	return &Client{
		MaxOutput:    maxOutput,
		StartTimeout: 5 * time.Second,
		Signal:       sigquit,
		Dial:         dialSocket,
		SocketExists: SocketExists,
		locks:        map[int]chan struct{}{},
	}
}

// SocketExists reports whether path is a Unix socket (not following symlinks).
func SocketExists(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// lock serializes commands per JVM (HotSpot handles one at a time anyway).
func (c *Client) lock(ctx context.Context, pid int) (func(), error) {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = map[int]chan struct{}{}
	}
	ch, ok := c.locks[pid]
	if !ok {
		ch = make(chan struct{}, 1)
		c.locks[pid] = ch
	}
	c.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ErrTimeout
	}
}

// Start starts the attach listener: trigger file first, then SIGQUIT, then
// wait for the socket. The caller must have checked that a signal is safe
// (no -Xrs, attach enabled, JVM older than 30 s); without the trigger file a
// JVM only prints a thread dump on SIGQUIT, with -Xrs it would terminate.
func (c *Client) Start(ctx context.Context, t Target) error {
	if c.SocketExists(t.Socket()) {
		return nil
	}
	trig := t.trigger()
	// O_EXCL never follows a symlink planted under that name.
	f, err := os.OpenFile(trig, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	switch {
	case err == nil:
		f.Close()
		defer os.Remove(trig)
	case errors.Is(err, os.ErrExist):
		// Another attacher is starting it; only an empty regular file is ours to use.
		if st, lerr := os.Lstat(trig); lerr != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("%w: unexpected %s", ErrProtocol, trig)
		}
	default:
		return fmt.Errorf("attach: create trigger: %w", err)
	}
	if err := c.Signal(t.HostPID); err != nil {
		return fmt.Errorf("attach: signal: %w", err)
	}
	timeout := c.StartTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if c.SocketExists(t.Socket()) {
			return nil
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return ErrTimeout
		case <-ctx.Done():
			return ErrTimeout
		}
	}
}

// Jcmd runs a diagnostic command (e.g. "Thread.print -l") and returns its output.
// The listener must be running (see Start).
func (c *Client) Jcmd(ctx context.Context, t Target, command string) (string, error) {
	return c.Execute(ctx, t, "jcmd", command)
}

// Execute sends one attach operation with up to three arguments (protocol 1).
func (c *Client) Execute(ctx context.Context, t Target, op string, args ...string) (string, error) {
	if len(args) > 3 {
		return "", fmt.Errorf("%w: too many arguments", ErrProtocol)
	}
	for _, s := range append([]string{op}, args...) {
		if strings.IndexByte(s, 0) >= 0 {
			return "", fmt.Errorf("%w: NUL in argument", ErrProtocol)
		}
	}
	unlock, err := c.lock(ctx, t.HostPID)
	if err != nil {
		return "", err
	}
	defer unlock()
	if !c.SocketExists(t.Socket()) {
		return "", ErrNotRunning
	}
	// Right after Start the socket can exist before the JVM listens on it.
	conn, err := c.Dial(ctx, t.Socket())
	for i := 0; err != nil && i < 5 && ctx.Err() == nil; i++ {
		time.Sleep(50 * time.Millisecond)
		conn, err = c.Dial(ctx, t.Socket())
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ErrTimeout
		}
		return "", fmt.Errorf("attach: connect: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	var req bytes.Buffer
	req.WriteString("1\x00" + op + "\x00")
	for i := 0; i < 3; i++ {
		if i < len(args) {
			req.WriteString(args[i])
		}
		req.WriteByte(0)
	}
	if _, err := conn.Write(req.Bytes()); err != nil {
		return "", c.ioErr(ctx, err)
	}

	r := bufio.NewReader(io.LimitReader(conn, c.MaxOutput+1))
	codeLine, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && codeLine == "" {
			return "", fmt.Errorf("%w: connection closed without result", ErrProtocol)
		}
		if !errors.Is(err, io.EOF) {
			return "", c.ioErr(ctx, err)
		}
	}
	code, err := strconv.Atoi(strings.TrimSpace(codeLine))
	if err != nil {
		return "", fmt.Errorf("%w: bad result %q", ErrProtocol, strings.TrimSpace(codeLine))
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return "", c.ioErr(ctx, err)
	}
	if int64(len(codeLine)+len(out)) > c.MaxOutput {
		return "", ErrTooLarge
	}
	if code != 0 {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return "", &CommandError{Code: code, Message: msg}
	}
	return string(out), nil
}

func (c *Client) ioErr(ctx context.Context, err error) error {
	var ne net.Error
	if ctx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()) {
		return ErrTimeout
	}
	return fmt.Errorf("attach: %w", err)
}
