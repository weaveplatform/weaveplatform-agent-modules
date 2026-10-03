package handshake

import (
	"errors"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/ipc"
)

func TestLineRoundTrip(t *testing.T) {
	l := Line{Protocol: 3, Network: "unix", Addr: "/var/run/weave/example.sock"}
	got, err := Parse(l.Format())
	if err != nil {
		t.Fatal(err)
	}
	if got != l {
		t.Fatalf("round trip: got %+v want %+v", got, l)
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"",
		"WEAVE|1|1|unix",
		"NOPE|1|1|unix|/a",
		"WEAVE|2|1|unix|/a",
		"WEAVE|1|0|unix|/a",
		"WEAVE|1|1|tcp|127.0.0.1:1",
		"WEAVE|1|1|unix|",
	}
	for _, s := range bad {
		if _, err := Parse(s); !errors.Is(err, ErrInvalidLine) {
			t.Errorf("Parse(%q): err = %v, want ErrInvalidLine", s, err)
		}
	}
}

func TestWindow(t *testing.T) {
	w, err := ParseWindow("2", "3")
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[uint32]bool{1: false, 2: true, 3: true, 4: false} {
		if w.Contains(p) != want {
			t.Errorf("Contains(%d) = %v, want %v", p, !want, want)
		}
	}
	if _, err := ParseWindow("0", "3"); !errors.Is(err, ErrInvalidWindow) {
		t.Error("window min 0 accepted")
	}
	if _, err := ParseWindow("3", "2"); !errors.Is(err, ErrInvalidWindow) {
		t.Error("inverted window accepted")
	}
}

// A protocol-1 module may print "winpipe" where this sdk prints "npipe", and
// core runs every protocol-1 module, so Parse must accept it. Only Windows
// modules print a pipe network; a unix guest would never show the failure.
func TestParseAcceptsTheWinpipeNetworkName(t *testing.T) {
	line, err := Parse(`WEAVE|1|1|winpipe|\\.\pipe\weave-module-x`)
	if err != nil {
		t.Fatalf("a module printing winpipe was refused: %v", err)
	}
	// Normalised, so nothing downstream has to know two spellings.
	if line.Network != ipc.NetworkPipe {
		t.Errorf("network = %q, want %q", line.Network, ipc.NetworkPipe)
	}
	if line.Addr != `\\.\pipe\weave-module-x` {
		t.Errorf("addr = %q", line.Addr)
	}
}

// The alias is exactly one name, not a general loosening.
func TestParseStillRefusesAnUnknownNetwork(t *testing.T) {
	if _, err := Parse(`WEAVE|1|1|tcp|127.0.0.1:9000`); !errors.Is(err, ErrInvalidLine) {
		t.Fatal("an unknown network was accepted")
	}
}
