package weavewire_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

func TestCommandAndResultRoundTrip(t *testing.T) {
	data, err := weavewire.EncodeCommand("c1", weavewire.PowerRequest{Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	var cmd weavewire.Command
	if err := json.Unmarshal(data, &cmd); err != nil || cmd.ID != "c1" {
		t.Fatalf("cmd = %+v, %v", cmd, err)
	}
	var req weavewire.PowerRequest
	if err := json.Unmarshal(cmd.Payload, &req); err != nil || req.Reason != "r" {
		t.Fatalf("req = %+v, %v", req, err)
	}

	// A nil payload is omitted, not encoded as null.
	data, err = weavewire.EncodeCommand("c2", nil)
	if err != nil || string(data) != `{"id":"c2"}` {
		t.Fatalf("data = %s, %v", data, err)
	}

	data, err = weavewire.EncodeResult("c1", weavewire.PowerResponse{Accepted: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var res weavewire.Result
	if err := json.Unmarshal(
		data,
		&res,
	); err != nil || res.ID != "c1" || res.Err != "" ||
		res.Code != "" {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

func TestResultsCarryFailuresAndTheirCode(t *testing.T) {
	data, err := weavewire.EncodeResult("x", nil, errors.New("boom"))
	if err != nil {
		t.Fatal(err)
	}
	var res weavewire.Result
	_ = json.Unmarshal(data, &res)
	if res.Err != "boom" || res.Code != "" || res.Payload != nil {
		t.Fatalf("res = %+v", res)
	}

	unsupported := fmt.Errorf(
		"wrapped: %w",
		&weavewire.UnsupportedError{
			Kind:   weavewire.KindTimeSet,
			Reason: "hypervisor owns the clock",
		},
	)
	data, _ = weavewire.EncodeResult("y", nil, unsupported)
	res = weavewire.Result{}
	_ = json.Unmarshal(data, &res)
	if res.Code != weavewire.CodeUnsupported {
		t.Fatalf("code = %q", res.Code)
	}
	if got := (&weavewire.UnsupportedError{Kind: "k"}).Error(); got != "k is not supported on this guest" {
		t.Fatalf("message = %q", got)
	}
}

func TestEncodingRefusesUnencodablePayloads(t *testing.T) {
	bad := map[string]any{"f": func() {}}
	if _, err := weavewire.EncodeCommand("c", bad); err == nil {
		t.Error("EncodeCommand")
	}
	if _, err := weavewire.EncodeResult("c", bad, nil); err == nil {
		t.Error("EncodeResult")
	}
	if _, err := weavewire.EncodeEvent(bad); err == nil {
		t.Error("EncodeEvent")
	}
	if data, err := weavewire.EncodeEvent(nil); err != nil || data != nil {
		t.Errorf("EncodeEvent(nil) = %s, %v", data, err)
	}
	if data, err := weavewire.EncodeEvent(
		weavewire.ExecExit{ExecID: "e"},
	); err != nil ||
		len(data) == 0 {
		t.Errorf("EncodeEvent = %s, %v", data, err)
	}
}

func TestResultKinds(t *testing.T) {
	if weavewire.ResultKind("a.b") != "a.b.result" {
		t.Fatal("ResultKind")
	}
	for kind, want := range map[string]bool{
		"weave.exec.start.result": true,
		"weave.exec.start":        false,
		".result":                 false,
		"":                        false,
	} {
		if weavewire.IsResult(kind) != want {
			t.Errorf("IsResult(%q) != %v", kind, want)
		}
	}
	if !weavewire.IsOrderedInbound(weavewire.KindExecStdin) ||
		weavewire.IsOrderedInbound(weavewire.KindExecStart) {
		t.Fatal("only stdin is ordered")
	}
}

func TestSkew(t *testing.T) {
	if got := (weavewire.TimeSetResponse{SkewNanos: int64(time.Second)}).Skew(); got != time.Second {
		t.Fatalf("skew = %v", got)
	}
}

// The decoders a guest runs on host-supplied bytes must never panic, whatever
// arrives.
func FuzzDecodeCommand(f *testing.F) {
	for _, seed := range []string{`{"id":"x","payload":{"argv":["a"]}}`, `{}`, `[]`, `{"id":1}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var cmd weavewire.Command
		if json.Unmarshal(data, &cmd) != nil {
			return
		}
		var req weavewire.ExecRequest
		_ = json.Unmarshal(cmd.Payload, &req)
		var chunk weavewire.Chunk
		if json.Unmarshal(cmd.Payload, &chunk) == nil {
			var a weavewire.StreamAssembler
			_, _ = a.Accept(chunk)
		}
	})
}
