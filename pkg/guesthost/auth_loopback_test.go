package guesthost_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guesthost"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestpresence"
)

// The host end of the handshake, against guestmoduletest.Core, which applies
// the same hvchannel gate and Verify that agent-core's transport applies. What
// this catches is drift between the two ends of a wire with no negotiation on
// it: a host that stopped signing what the guest checks would show up here
// rather than as a guest that mysteriously ignores every command.

func standIn(t *testing.T, trusted ed25519.PublicKey) *guesthost.Client {
	t.Helper()
	return wireWith(t, trusted, guestpresence.NewService(fakeKernel{}, nil))
}

func TestAuthenticateWithTheVMKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client := standIn(t, pub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Hello answers before authentication — it is how a host tells "wrong
	// key" from "no agent" — and inventory does not.
	if _, err := client.Hello(ctx); err != nil {
		t.Fatalf("pre-auth hello: %v", err)
	}
	if _, err := client.Inventory(ctx); !errors.Is(err, guesthost.ErrNotAuthenticated) {
		t.Fatalf("pre-auth inventory: err = %v, want ErrNotAuthenticated", err)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, err := client.Inventory(ctx); err != nil {
		t.Fatalf("inventory after authenticating: %v", err)
	}
}

func TestAuthenticateRefusesANonEd25519Key(t *testing.T) {
	client := standIn(t, nil)
	if err := client.Authenticate(context.Background(), ed25519.PrivateKey("short")); err == nil {
		t.Fatal("a malformed key was accepted")
	}
}

// The wrong key must produce an error promptly. A host that hung here would be
// indistinguishable from one talking to a guest that has no agent — which is the
// exact confusion the pre-auth hello exemption exists to prevent.
func TestAuthenticateReportsARefusalRatherThanHanging(t *testing.T) {
	trusted, _, _ := ed25519.GenerateKey(rand.Reader)
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	client := standIn(t, trusted)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Authenticate(ctx, wrongPriv)
	if err == nil {
		t.Fatal("authenticating with the wrong key succeeded")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the refusal was waited out instead of reported")
	}
}

// A refusal that arrives while a call is in flight must fail that call, not leave
// it waiting for a reply the guest has already declined to send.
func TestRefusedOperationFailsTheCallInFlight(t *testing.T) {
	trusted, _, _ := ed25519.GenerateKey(rand.Reader)
	client := standIn(t, trusted)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Inventory(ctx)
	if err == nil {
		t.Fatal("an unauthenticated channel returned an inventory")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the call timed out instead of being failed by the refusal")
	}
	if !errors.Is(err, guesthost.ErrNotAuthenticated) {
		t.Fatalf("error = %v, want ErrNotAuthenticated", err)
	}
}
