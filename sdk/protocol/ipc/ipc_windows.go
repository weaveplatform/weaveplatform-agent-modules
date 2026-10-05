package ipc

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const network = NetworkPipe

// pipeSDDL restricts the pipe to SYSTEM and the Administrators group:
//
//	D:  discretionary ACL
//	(A;;GA;;;SY)  allow generic-all to SYSTEM
//	(A;;GA;;;BA)  allow generic-all to Builtin Administrators
//
// This replaces go-winio's default (which grants broad access), closing
// the "any local user can open the control/host pipe" hole. Listen adds the
// listening process's own user to it (see ownPipeSDDL).
const pipeSDDL = "D:(A;;GA;;;SY)(A;;GA;;;BA)"

// Seam over the process token's user, for the SYSTEM and failure branches.
var tokenUser = func() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("ipc: reading the process token's user: %w", err)
	}
	return u.User.Sid, nil
}

func listen(addr string) (net.Listener, error) {
	sddl, err := ownPipeSDDL()
	if err != nil {
		return nil, err
	}
	return ListenPipeSDDL(addr, sddl)
}

// ownPipeSDDL is pipeSDDL plus generic-all for the user this process runs
// as. A listener must be able to open its own pipe: go-winio creates the
// first instance with the descriptor, then every Accept opens a further
// instance of that pipe for read and write, which is checked against the
// descriptor like any other open. A module core launched as the console
// user (privilege "user") is neither SYSTEM nor, under UAC, an
// Administrator, so with pipeSDDL alone its first Accept fails with "Access
// is denied", the gRPC server stops, the pipe goes, and core's dial finds
// nothing there. The user gains nothing by it: the module process is
// already theirs. Everyone else stays out.
func ownPipeSDDL() (string, error) {
	sid, err := tokenUser()
	if err != nil {
		return "", err
	}
	if sid.IsWellKnown(windows.WinLocalSystemSid) {
		return pipeSDDL, nil
	}
	return pipeSDDL + "(A;;GA;;;" + sid.String() + ")", nil
}

// ListenPipeSDDL creates a named-pipe listener with an explicit SDDL, for
// callers (privilege-dropped modules) that need a wider descriptor than
// the SYSTEM+Administrators default.
func ListenPipeSDDL(addr, sddl string) (net.Listener, error) {
	l, err := winio.ListenPipe(addr, &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		return nil, fmt.Errorf("ipc: listening on %s: %w", addr, err)
	}
	return &pipeListener{Listener: l, closed: make(chan struct{})}, nil
}

// pipeCloseTimeout bounds how long Close waits for go-winio to let go.
const pipeCloseTimeout = 2 * time.Second

// pipeListener works around a go-winio (v0.6.2) race that can make Close
// block forever. Close hands its signal to whatever is reading the listener's
// close channel; when that is an Accept still waiting for a client, the
// aborted connect can come back as an error other than ErrFileClosed, so the
// listener goroutine treats it as an ordinary failed Accept, never exits, and
// Close waits on it for good. gRPC calls Close from Stop and GracefulStop
// while holding its server mutex, and the Serve goroutine then needs that
// mutex to handle the failed Accept: the module never exits. Seen on
// windows-latest as a ten-minute test timeout.
//
// Close here is therefore bounded and idempotent, and Accept refuses once
// Close has begun. On timeout the go-winio goroutine and the pipe's first
// handle leak, which is preferable to a module that cannot shut down.
type pipeListener struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
	}
	c, err := l.Listener.Accept()
	if err != nil {
		select {
		case <-l.closed:
			return nil, net.ErrClosed
		default:
		}
		// Unwrapped on purpose: gRPC's Serve type-asserts an Accept error
		// for Temporary() to decide between retrying and exiting, and a
		// wrapper would hide that method.
		return nil, err //nolint:wrapcheck // net.Listener contract, see above
	}
	return c, nil
}

func (l *pipeListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		done := make(chan struct{})
		go func() {
			l.Listener.Close() //nolint:errcheck // go-winio's Close always returns nil
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(pipeCloseTimeout):
		}
	})
	return nil
}

func dial(ctx context.Context, netw, addr string) (net.Conn, error) {
	if netw != NetworkPipe {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedNetwork, netw)
	}
	c, err := winio.DialPipeContext(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("ipc: dialing %s: %w", addr, err)
	}
	return c, nil
}
