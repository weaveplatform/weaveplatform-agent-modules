package config

import "testing"

type cfg struct {
	Speed string `json:"speed"`
	Count int    `json:"count"`
}

func TestLoadEmptyLeavesDefaults(t *testing.T) {
	c := cfg{Speed: "slow", Count: 3}
	for _, doc := range [][]byte{nil, {}} {
		if err := Load(doc, &c); err != nil {
			t.Fatal(err)
		}
	}
	if c != (cfg{Speed: "slow", Count: 3}) {
		t.Fatalf("defaults changed: %+v", c)
	}
}

func TestLoadOverlaysDocumentOnDefaults(t *testing.T) {
	c := cfg{Speed: "slow", Count: 3}
	if err := Load([]byte(`{"speed":"fast"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c != (cfg{Speed: "fast", Count: 3}) {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadRejectsMalformedDocument(t *testing.T) {
	var c cfg
	if err := Load([]byte(`{"speed":`), &c); err == nil {
		t.Fatal("malformed document accepted")
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("WEAVE_CONFIG_TEST_SET", "value")
	if got := Env("WEAVE_CONFIG_TEST_SET", "fallback"); got != "value" {
		t.Errorf("set: got %q", got)
	}
	// Set-but-empty is a deliberate value, not an absence.
	t.Setenv("WEAVE_CONFIG_TEST_EMPTY", "")
	if got := Env("WEAVE_CONFIG_TEST_EMPTY", "fallback"); got != "" {
		t.Errorf("empty: got %q", got)
	}
	if got := Env("WEAVE_CONFIG_TEST_UNSET_8f3a", "fallback"); got != "fallback" {
		t.Errorf("unset: got %q", got)
	}
}
