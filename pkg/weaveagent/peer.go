package weaveagent

import (
	"context"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/modulesdk"
)

type peerKey struct{}

// WithPeer records which peer a request came from: the host channel (a
// hypervisor or container host directly outside the machine) or GateWeave (the
// platform). The dispatcher sets it on every request, so the reply and every
// event the request causes — an exec's output, its exit — go back the way the
// request came, whichever that was.
func WithPeer(ctx context.Context, p modulesdk.Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerFrom is the peer recorded by WithPeer. Without one it is the host
// channel: an event no request caused belongs to the host directly outside.
func PeerFrom(ctx context.Context) modulesdk.Peer {
	if p, ok := ctx.Value(peerKey{}).(modulesdk.Peer); ok && p != 0 {
		return p
	}
	return modulesdk.PeerHypervisor
}
