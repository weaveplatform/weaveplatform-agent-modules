package handshake

import (
	"errors"
	"testing"
)

func TestParseWindowRejectsNonNumbers(t *testing.T) {
	for _, c := range [][2]string{{"", "1"}, {"one", "1"}, {"1", ""}, {"1", "-2"}, {"1", "4294967296"}} {
		if _, err := ParseWindow(c[0], c[1]); !errors.Is(err, ErrInvalidWindow) {
			t.Errorf("ParseWindow(%q, %q) accepted", c[0], c[1])
		}
	}
}
