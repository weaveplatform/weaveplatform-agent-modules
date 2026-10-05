package weaveclient_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
)

// What a modules.list timeout means depends on what the connection has
// shown about core: silence from a core never seen to have a registry reads
// as an older core, silence from one that has shown it reads as a channel
// that is not moving.

const registryWait = 50 * time.Millisecond

// control writes one control frame from the guest.
func (g *rawGuest) control(kind, id string, payload any) {
	g.t.Helper()
	data, _ := json.Marshal(payload)
	g.write(hvchannel.Envelope{Module: hvchannel.ControlModule, Kind: kind, Data: data, ID: id})
}

// unanswered runs Modules, reads its modules.list and leaves it unanswered,
// returning the list's id and the error.
func unanswered(t *testing.T, guest *rawGuest, client *weaveclient.Client) (string, error) {
	t.Helper()
	errs := make(chan error, 1)
	go func() { _, err := client.Modules(timeout(t)); errs <- err }()
	list := guest.read()
	if list.Kind != hvchannel.KindModulesList {
		t.Fatalf("sent %s, want modules.list", list.Kind)
	}
	return list.ID, <-errs
}

func wantUnsupported(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, weaveclient.ErrRegistryUnsupported) ||
		errors.Is(err, weaveclient.ErrRegistryTimeout) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrRegistryUnsupported", err)
	}
}

func wantRegistryTimeout(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, weaveclient.ErrRegistryTimeout) ||
		errors.Is(err, weaveclient.ErrRegistryUnsupported) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrRegistryTimeout", err)
	}
}

// awaitSnapshot waits for the client to hold a snapshot at rev or later.
func awaitSnapshot(t *testing.T, client *weaveclient.Client, rev uint64) {
	t.Helper()
	deadline := time.Now().Add(quick)
	for time.Now().Before(deadline) {
		if s, ok := client.Snapshot(); ok && s.Revision >= rev {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no snapshot at revision %d", rev)
}

// A fresh connection to a core that has shown nothing is read as an older
// core; once core has pushed a snapshot, the same silence is a stalled
// channel.
func TestRegistryTimeoutOnceCoreHasShownARegistry(t *testing.T) {
	guest, client := newRawWith(t, weaveclient.Options{RegistryTimeout: registryWait})

	_, err := unanswered(t, guest, client)
	wantUnsupported(t, err)

	guest.control(hvchannel.KindModulesChanged, "", weaveclient.ModulesSnapshot{Revision: 1})
	awaitSnapshot(t, client, 1)

	_, err = unanswered(t, guest, client)
	wantRegistryTimeout(t, err)
	if !strings.Contains(err.Error(), "not moving") {
		t.Fatalf("err = %v", err)
	}
}

// "Unsupported" is a conclusion about one call: an answer arriving after
// it gave up still lands, and revises what the next timeout means.
func TestRegistryConclusionIsRevisedByALateAnswer(t *testing.T) {
	guest, client := newRawWith(t, weaveclient.Options{RegistryTimeout: registryWait})

	id, err := unanswered(t, guest, client)
	wantUnsupported(t, err)

	guest.control(hvchannel.KindModulesListResult, id, weaveclient.ModulesSnapshot{Revision: 4})
	awaitSnapshot(t, client, 4)

	_, err = unanswered(t, guest, client)
	wantRegistryTimeout(t, err)
}

// Every frame that only a core with the registry sends is evidence of one.
func TestRegistryEvidenceFromOtherFrames(t *testing.T) {
	for name, send := range map[string]func(*rawGuest){
		"delivery.failed": func(g *rawGuest) {
			g.control(hvchannel.KindDeliveryFailed, "nobody", hvchannel.DeliveryFailed{
				Module: "weave.x", Kind: "weave.x.y", Reason: hvchannel.ReasonNotInstalled,
			})
		},
		"refusal with an id": func(g *rawGuest) {
			g.control(hvchannel.KindAuthResult, "nobody", hvchannel.AuthResult{Reason: "no"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			guest, client := newRawWith(t, weaveclient.Options{RegistryTimeout: registryWait})
			send(guest)
			_, err := unanswered(t, guest, client)
			wantRegistryTimeout(t, err)
		})
	}
}

// What the caller knows of core's version counts before the wire has said
// anything; anything the wire then says overrides it.
func TestRegistryEvidenceFromTheCoreVersion(t *testing.T) {
	for _, c := range []struct {
		version string
		timeout bool
		predate bool
	}{
		{"v0.9.2", true, false},
		{"0.9.2", true, false},
		{"v0.10.0", true, false},
		{"v1.0.0-rc.1+abc", true, false},
		{"v0.9.1", false, true},
		{"v0.8.9", false, true},
		{"", false, false},
		{"v0.9", false, false},
		{"v0.x.2", false, false},
		{"latest", false, false},
	} {
		t.Run(c.version, func(t *testing.T) {
			guest, client := newRawWith(t, weaveclient.Options{
				RegistryTimeout: registryWait, CoreVersion: c.version,
			})
			_, err := unanswered(t, guest, client)
			if c.timeout {
				wantRegistryTimeout(t, err)
				return
			}
			wantUnsupported(t, err)
			if predates := strings.Contains(err.Error(), "predates"); predates != c.predate {
				t.Fatalf("err = %v", err)
			}
			if !c.predate {
				return
			}
			// The wire contradicts the version the caller believed.
			guest.control(
				hvchannel.KindModulesChanged,
				"",
				weaveclient.ModulesSnapshot{Revision: 1},
			)
			awaitSnapshot(t, client, 1)
			_, err = unanswered(t, guest, client)
			wantRegistryTimeout(t, err)
		})
	}
}

// A fresh authentication may be to a different core, so what the wire
// showed about the old one is dropped — back to the caller's version, if it
// gave one.
func TestReauthenticationForgetsRegistryEvidence(t *testing.T) {
	for _, c := range []struct {
		version string
		timeout bool
	}{
		{"", false},
		{"v0.9.2", true},
	} {
		t.Run("version "+c.version, func(t *testing.T) {
			guest, client := newRawWith(t, weaveclient.Options{
				RegistryTimeout: registryWait, CoreVersion: c.version,
			})
			guest.control(
				hvchannel.KindModulesChanged,
				"",
				weaveclient.ModulesSnapshot{Revision: 1},
			)
			awaitSnapshot(t, client, 1)

			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			authed := make(chan error, 1)
			go func() { authed <- client.Authenticate(timeout(t), priv) }()
			guest.handshake()
			if err := <-authed; err != nil {
				t.Fatal(err)
			}
			if refresh := guest.read(); refresh.Kind != hvchannel.KindModulesList {
				t.Fatalf("sent %s after authenticating", refresh.Kind)
			}

			_, err := unanswered(t, guest, client)
			if c.timeout {
				wantRegistryTimeout(t, err)
			} else {
				wantUnsupported(t, err)
			}
		})
	}
}
