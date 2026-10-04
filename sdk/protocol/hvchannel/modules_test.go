package hvchannel

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The registry frames byte for byte as agent-core's docs/PROTOCOL.md shows
// them. Nothing negotiates this wire, so a renamed tag here would only show
// up as a host that never sees a module.
func TestModulesSnapshotWireShape(t *testing.T) {
	const doc = `{"revision":7,"modules":[` +
		`{"id":"weave-linux-clipboard","version":"0.4.0","protocol":1,"address":"weave.clipboard",` +
		`"capabilities":[],"privilege":"user","session":"per-user-console",` +
		`"state":"waiting-for-session","detail":"no console user session",` +
		`"health":{"status":"unknown"},"restarts":0,"since":"2026-10-04T11:58:12.031Z"},` +
		`{"id":"weave-linux-power","version":"1.2.3","protocol":1,"address":"weave.power",` +
		`"capabilities":["hypervisor.channel"],"privilege":"system","session":"system",` +
		`"state":"running","health":{"status":"healthy","reason":"ok"},"restarts":2,` +
		`"since":"2026-10-04T12:00:00Z"}]}`
	var snap ModulesSnapshot
	if err := json.Unmarshal([]byte(doc), &snap); err != nil {
		t.Fatal(err)
	}
	want := ModulesSnapshot{Revision: 7, Modules: []ModuleInfo{
		{
			ID: "weave-linux-clipboard", Version: "0.4.0", Protocol: 1,
			Address: "weave.clipboard", Capabilities: []string{}, Privilege: "user",
			Session: "per-user-console", State: "waiting-for-session",
			Detail: "no console user session", Health: ModuleHealth{Status: HealthUnknown},
			Since: "2026-10-04T11:58:12.031Z",
		},
		{
			ID: "weave-linux-power", Version: "1.2.3", Protocol: 1, Address: "weave.power",
			Capabilities: []string{"hypervisor.channel"}, Privilege: "system",
			Session: "system", State: "running",
			Health:   ModuleHealth{Status: HealthHealthy, Reason: "ok"},
			Restarts: 2, Since: "2026-10-04T12:00:00Z",
		},
	}}
	if !reflect.DeepEqual(snap, want) {
		t.Fatalf("decoded %+v\nwant    %+v", snap, want)
	}
	out, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != doc {
		t.Fatalf("re-encoded\n%s\nwant\n%s", out, doc)
	}
}

func TestDeliveryFailedWireShape(t *testing.T) {
	for _, doc := range []string{
		`{"module":"weave.power","kind":"weave.power.shutdown","reason":"not_installed"}`,
		`{"module":"weave.clipboard","kind":"weave.clipboard.get","reason":"not_running",` +
			`"state":"waiting-for-session","detail":"no console user session"}`,
		`{"module":"weave.exec","kind":"weave.exec.run","reason":"busy"}`,
	} {
		var df DeliveryFailed
		if err := json.Unmarshal([]byte(doc), &df); err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(df)
		if string(out) != doc {
			t.Errorf("round trip\n%s\nwant\n%s", out, doc)
		}
	}
	for _, reason := range []string{ReasonNotInstalled, ReasonNotRunning, ReasonBusy} {
		if reason == "" {
			t.Error("empty reason constant")
		}
	}
}

// The envelope id is omitted when empty, so a peer from before it existed
// sees exactly the frames it always did, and it survives a round trip when
// set.
func TestEnvelopeID(t *testing.T) {
	plain, _ := json.Marshal(Envelope{Module: ControlModule, Kind: KindModulesList})
	if string(plain) != `{"module":"hvchannel","kind":"modules.list"}` {
		t.Fatalf("no-id envelope = %s", plain)
	}
	var buf bytes.Buffer
	want := Envelope{
		Module: "weave.power",
		Kind:   "weave.power.shutdown",
		Data:   []byte("{}"),
		ID:     "a1b2c3-7",
	}
	if err := WriteEnvelope(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEnvelope(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if KindModulesListResult == KindModulesChanged || KindDeliveryFailed == "" {
		t.Fatal("control kinds collide")
	}
}
