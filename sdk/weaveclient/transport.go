package weaveclient

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
)

// Dialer opens the host end of one guest's hypervisor channel. The CLI supplies
// it: an HvSocket dial to the VM's service id on Windows, the VZ console pipe
// pair on macOS, anything that yields a byte stream to core inside the guest.
//
// A net.Conn is an io.ReadWriteCloser, so a socket-backed dialer returns its
// connection unchanged.
type Dialer interface {
	Dial(ctx context.Context) (io.ReadWriteCloser, error)
}

// DialerFunc adapts a function to Dialer.
type DialerFunc func(ctx context.Context) (io.ReadWriteCloser, error)

// Dial implements Dialer.
func (f DialerFunc) Dial(ctx context.Context) (io.ReadWriteCloser, error) { return f(ctx) }

// Dial opens a channel with d and wraps it in a Client. When key is non-nil
// the channel is authenticated before Dial returns, and a refusal closes it:
// a caller handed an unauthenticated client would find every op but hello
// refused, which is a worse place to discover a wrong key than here.
//
// ctx bounds the dial and the handshake only. The client lives until Close or
// until the channel ends.
func Dial(ctx context.Context, d Dialer, key ed25519.PrivateKey, opts Options) (*Client, error) {
	rwc, err := d.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("weaveclient: dialing the channel: %w", err)
	}
	c := New(context.WithoutCancel(ctx), rwc, opts)
	if key == nil {
		return c, nil
	}
	if err := c.Authenticate(ctx, key); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Pipes joins a read end and a write end into one channel connection, for
// transports that expose the two directions separately — the pipe pair bridged
// into a Virtualization.framework console port is the case this exists for.
// Close closes both ends and reports the first failure.
func Pipes(r io.ReadCloser, w io.WriteCloser) io.ReadWriteCloser {
	return &pipePair{r: r, w: w}
}

type pipePair struct {
	r io.ReadCloser
	w io.WriteCloser
}

// Read and Write pass errors through untouched: io.EOF must stay io.EOF for
// the framing reader to tell a clean close from a torn frame.

//nolint:wrapcheck // see above
func (p *pipePair) Read(b []byte) (int, error) { return p.r.Read(b) }

//nolint:wrapcheck // see above
func (p *pipePair) Write(b []byte) (int, error) { return p.w.Write(b) }

// Close closes the write end first, so the guest sees end of input before the
// host stops listening for whatever it sends back.
func (p *pipePair) Close() error {
	return errors.Join(p.w.Close(), p.r.Close())
}
