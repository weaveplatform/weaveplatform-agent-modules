package weaveclient

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
)

// The host end of channel authentication: prove possession of this VM's private
// key so the guest will honour anything beyond hello.
//
// The guest holds the public half, placed in its image at build time; the host
// holds the private half in the VM's directory. See the sdk's hvchannel package
// for the handshake and the reasoning about scope — one key per VM, so a process
// able to drive one guest cannot drive its neighbour.

// ErrNotAuthenticated reports that the guest refused an operation because this
// channel never proved who it was. It is distinct from a timeout on purpose: a
// caller that cannot tell "refused" from "no agent" ends up retrying against a
// guest that will never answer.
var ErrNotAuthenticated = errors.New(
	"weaveclient: the guest refused the operation on an unauthenticated channel",
)

// ErrAuthRefused reports that the guest rejected the handshake itself: the
// wrong key for this VM, or a guest with no key provisioned.
var ErrAuthRefused = errors.New("weaveclient: the guest refused authentication")

var errNotEd25519 = errors.New("weaveclient: channel private key is not ed25519")

// Authenticate performs the handshake. Call it once, immediately after New and
// before any operation other than Hello.
//
// A refusal comes back as an error rather than a silent downgrade: a host that
// carried on unauthenticated would work for exactly one operation (hello) and
// then fail confusingly on every other, which is the shape of bug that gets
// diagnosed as "the guest agent is flaky".
func (c *Client) Authenticate(ctx context.Context, priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return errNotEd25519
	}

	// Register as the control-frame waiter BEFORE the first send. The guest's
	// reply can reach the read loop before this goroutine gets back to a
	// receive — on a synchronous pipe it always does — and a reply that arrives
	// with no registered waiter is treated as an unsolicited refusal, which
	// would fail the handshake it was actually answering.
	c.beginControlWait()
	defer c.endControlWait()

	if err := c.sendControl(hvchannel.KindAuthBegin, "", nil); err != nil {
		return err
	}
	challengeEnv, err := c.awaitControl(ctx, hvchannel.KindAuthChallenge)
	if err != nil {
		return err
	}
	var challenge hvchannel.AuthChallenge
	if err := json.Unmarshal(challengeEnv.Data, &challenge); err != nil {
		return fmt.Errorf("weaveclient: undecodable challenge: %w", err)
	}

	resp, err := hvchannel.Sign(priv, challenge)
	if err != nil {
		return fmt.Errorf("weaveclient: answering the challenge: %w", err)
	}
	if err := c.sendControl(hvchannel.KindAuthResponse, "", resp); err != nil {
		return err
	}

	resultEnv, err := c.awaitControl(ctx, hvchannel.KindAuthResult)
	if err != nil {
		return err
	}
	var result hvchannel.AuthResult
	if err := json.Unmarshal(resultEnv.Data, &result); err != nil {
		return fmt.Errorf("weaveclient: undecodable auth result: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("%w: %s", ErrAuthRefused, result.Reason)
	}
	// A fresh authentication may be a fresh core — a guest whose agent
	// restarted behind the same channel, with its registry revision back at
	// 1 — so the snapshot was dropped when the result arrived (routeControl)
	// and is fetched again now, as core's protocol asks of a host that wants
	// a complete view. In the background: an older core never answers, and
	// authenticating must not wait out that silence.
	go c.refreshModules(context.WithoutCancel(ctx))
	return nil
}

// sendControl writes one frame addressed to the channel itself rather than to a
// module. It bypasses the module addressing in send because control frames are
// not module traffic and must not be mistaken for it at either end. id, when
// set, is echoed on core's reply.
func (c *Client) sendControl(kind, id string, payload any) error {
	var data []byte
	if payload != nil {
		var err error
		if data, err = json.Marshal(payload); err != nil {
			return fmt.Errorf("weaveclient: encoding %s: %w", kind, err)
		}
	}
	c.mu.Lock()
	if c.closed {
		err := c.cause
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrame(
		hvchannel.Envelope{Module: hvchannel.ControlModule, Kind: kind, Data: data, ID: id},
	)
}

// awaitControl waits for one control frame of the given kind.
//
// A refusal arriving where a challenge was expected is reported as such rather
// than waited out: an unprovisioned guest replies to auth.begin with a failed
// result, and a host that kept waiting would look like a host talking to a guest
// that is merely slow to boot.
// beginControlWait and endControlWait bracket a handshake, so routeControl can
// tell a frame this client asked for from one the guest sent unprompted. Buffer
// capacity cannot stand in for that: an empty buffer says nothing about whether
// anyone will ever read it.
func (c *Client) beginControlWait() {
	c.mu.Lock()
	c.awaitingControl++
	c.mu.Unlock()
}

func (c *Client) endControlWait() {
	c.mu.Lock()
	c.awaitingControl--
	c.mu.Unlock()
}

func (c *Client) awaitControl(ctx context.Context, want string) (hvchannel.Envelope, error) {
	for {
		select {
		case env := <-c.control:
			if env.Kind == want {
				return env, nil
			}
			if env.Kind == hvchannel.KindAuthResult {
				var result hvchannel.AuthResult
				_ = json.Unmarshal(env.Data, &result)
				if !result.OK {
					return hvchannel.Envelope{}, fmt.Errorf("%w: %s", ErrAuthRefused, result.Reason)
				}
			}
			c.log.Debug("weaveclient: control frame out of turn", "kind", env.Kind, "want", want)
		case <-ctx.Done():
			return hvchannel.Envelope{}, ctx.Err()
		case <-c.done:
			return hvchannel.Envelope{}, c.Err()
		}
	}
}

// routeControl handles a control frame from the read loop.
//
// The registry and undeliverable-message frames are answers to, or news for,
// whoever is interested regardless of any handshake, so they are routed
// first and never reach the handshake waiter. An auth.result that echoes an
// id is core refusing that one frame on an unauthenticated channel: it fails
// that call alone. Anything else a handshake might be waiting for goes to it.
//
// A refusal with no id and nobody waiting comes from a core older than
// weave-agent v0.9.2, which does not echo ids: it means the guest just
// rejected a module operation on an unauthenticated channel, and with no way
// to tell which, every call in flight is failed immediately rather than left
// to time out, because the timeout would be indistinguishable from a guest
// that is not there.
func (c *Client) routeControl(env hvchannel.Envelope) {
	switch env.Kind {
	case hvchannel.KindModulesListResult, hvchannel.KindModulesChanged:
		c.routeModules(env)
		return
	case hvchannel.KindDeliveryFailed:
		c.routeDeliveryFailed(env)
		return
	case hvchannel.KindAuthResult:
		if env.ID != "" {
			c.routeRefusal(env)
			return
		}
	}

	c.mu.Lock()
	waiting := c.awaitingControl > 0
	c.mu.Unlock()

	if waiting {
		if env.Kind == hvchannel.KindAuthResult {
			var result hvchannel.AuthResult
			if json.Unmarshal(env.Data, &result) == nil && result.OK {
				c.resetModules()
			}
		}
		select {
		case c.control <- env:
			return
		default:
			// The waiter is alive but behind. Dropping is right: it is waiting
			// for one specific frame, and a full buffer means the guest is
			// sending control frames faster than a handshake consumes them.
			c.log.Warn(
				"weaveclient: control frame dropped, handshake buffer full",
				"kind",
				env.Kind,
			)
			return
		}
	}

	if env.Kind != hvchannel.KindAuthResult {
		// A control kind this client does not know, from a newer core: the
		// channel changes additively, so it is not an error.
		c.log.Debug("weaveclient: unhandled control frame", "kind", env.Kind)
		return
	}
	var result hvchannel.AuthResult
	_ = json.Unmarshal(env.Data, &result)
	if result.OK {
		return
	}
	c.log.Warn("weaveclient: the guest refused an operation on an unauthenticated channel",
		"reason", result.Reason)
	c.failPending(ErrNotAuthenticated)
}

// routeRefusal fails the one call whose frame core refused.
func (c *Client) routeRefusal(env hvchannel.Envelope) {
	// Only a core with the registry echoes ids (v0.9.2).
	c.sawRegistry()
	var result hvchannel.AuthResult
	_ = json.Unmarshal(env.Data, &result)
	if result.OK {
		return
	}
	c.log.Warn("weaveclient: the guest refused an operation on an unauthenticated channel",
		"reason", result.Reason, "id", env.ID)
	if !c.answer(env.ID, pendingReply{
		err: fmt.Errorf("%w: %s", ErrNotAuthenticated, result.Reason),
	}) {
		c.log.Debug("weaveclient: refusal for a call no longer waiting", "id", env.ID)
	}
}

// failPending releases every waiting call with cause, leaving the channel open.
// Used where the channel is fine but the guest will not act — the calls must not
// wait for a reply that is never coming.
func (c *Client) failPending(cause error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan pendingReply)
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- pendingReply{err: cause}:
		default:
		}
	}
}
