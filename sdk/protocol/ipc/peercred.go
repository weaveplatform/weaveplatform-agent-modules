package ipc

import "net"

// PeerCred is the identity of the process on the other end of a local
// connection, as the OS reports it. HasUID is false on platforms/paths
// where a uid cannot be determined (e.g. Windows named pipes, where
// access is gated by the pipe SDDL instead).
type PeerCred struct {
	UID    uint32
	HasUID bool
	PID    int
	HasPID bool
}

// Authorizer decides whether a connecting peer may be served. Returning a
// non-nil error causes the connection to be closed before it reaches the
// gRPC server.
type Authorizer func(PeerCred) error

// ListenAuthorized wraps Listen with a peer-credential gate: every accepted
// connection's OS-reported peer identity is checked by authorize before it
// is handed on. This is the honest local-IPC control the token was only
// pretending to be — a same-uid process can read another's env token, but
// it cannot forge its own uid to the kernel.
func ListenAuthorized(addr string, authorize Authorizer) (net.Listener, error) {
	l, err := Listen(addr)
	if err != nil {
		return nil, err
	}
	return &authListener{Listener: l, authorize: authorize}, nil
}

type authListener struct {
	net.Listener
	authorize Authorizer
}

func (l *authListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			// Unwrapped on purpose: gRPC's Serve type-asserts an Accept
			// error for Temporary() to decide between retrying and exiting,
			// and a wrapper would hide that method.
			return nil, err //nolint:wrapcheck // net.Listener contract, see above
		}
		if l.authorize == nil {
			return c, nil
		}
		if aerr := l.authorize(peerCred(c)); aerr != nil {
			c.Close() //nolint:errcheck
			continue  // reject and wait for the next connection
		}
		return c, nil
	}
}
