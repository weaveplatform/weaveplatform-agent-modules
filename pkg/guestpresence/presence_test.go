package guestpresence_test

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule/guestmoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestpresence"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

type fakeKernel struct {
	name string
	err  error
}

func (k fakeKernel) Kernel(context.Context) (string, error) { return k.name, k.err }

func TestServesExactlyThePresenceContract(t *testing.T) {
	if err := guestmodule.CheckParity(
		guestpresence.NewService(fakeKernel{}, silentInventory{}),
	); err != nil {
		t.Fatal(err)
	}
}

// Core lets exactly one module op through before the channel authenticates,
// and recognises it by spelling. If presence's hello drifted from that
// spelling, an unprovisioned guest would look like a guest with no agent.
func TestHelloIsTheKindCoreAnswersBeforeAuthentication(t *testing.T) {
	if guestwire.KindPresenceHello != hvchannel.PreAuthKind {
		t.Fatalf("hello is %q, core exempts %q", guestwire.KindPresenceHello, hvchannel.PreAuthKind)
	}
	if !hvchannel.AllowedBeforeAuth(guestwire.ResultKind(guestwire.KindPresenceHello)) {
		t.Fatal("core would refuse the hello reply")
	}
	if hvchannel.AllowedBeforeAuth(guestwire.KindPresenceInventory) {
		t.Fatal("inventory must not cross an unauthenticated channel")
	}
}

func TestHelloReportsVersionPlatformAndKernel(t *testing.T) {
	h := guestmoduletest.Start(t, guestpresence.NewService(fakeKernel{name: "test-kernel"}, nil))
	var hello guestwire.HelloResponse
	h.Decode(guestwire.KindPresenceHello, nil, &hello)
	if hello.Version != guestwire.ProtocolVersion {
		t.Fatalf("version = %q, want %q", hello.Version, guestwire.ProtocolVersion)
	}
	if hello.OS != runtime.GOOS || hello.Arch != runtime.GOARCH {
		t.Fatalf("os/arch = %s/%s", hello.OS, hello.Arch)
	}
	if hello.Kernel != "test-kernel" {
		t.Fatalf("kernel = %q, the backend was not consulted", hello.Kernel)
	}
	if want, _ := os.Hostname(); hello.Hostname != want {
		t.Fatalf("hostname = %q, want %q", hello.Hostname, want)
	}
}

// Hello is the liveness probe. A kernel string the OS will not give up must
// not turn "the agent is here" into "the agent is broken".
func TestHelloSurvivesWithoutAKernel(t *testing.T) {
	for _, svc := range []*guestpresence.Service{
		guestpresence.NewService(nil, nil),
		guestpresence.NewService(fakeKernel{err: errors.New("uname failed")}, nil),
	} {
		var hello guestwire.HelloResponse
		guestmoduletest.Start(t, svc).Decode(guestwire.KindPresenceHello, nil, &hello)
		if hello.Version == "" || hello.Kernel != "" {
			t.Fatalf("hello = %+v", hello)
		}
	}
}

// With no OS backend the portable inventory still answers.
func TestInventoryWithoutABackend(t *testing.T) {
	var inv guestwire.InventoryResponse
	guestmoduletest.Start(t, guestpresence.NewService(nil, nil)).
		Decode(guestwire.KindPresenceInventory, nil, &inv)
	if inv.OS != runtime.GOOS || len(inv.Interfaces) == 0 {
		t.Fatalf("inventory = %+v", inv)
	}
}
