//go:build unix

// Concurrency around the pseudo-terminal, which the ordinary exec tests do not
// exercise: they resize a live session, never one that is ending.

package weaveexec

import (
	"context"
	"sync"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Resize and Close race by construction: a terminal resize comes from the
// operator's SIGWINCH, the close from the process exiting, and an operator
// dragging a window while a command finishes hits both at once.
//
// Found by the race detector's first run against this package. Before the fix
// they raced on the file's own state — and after the descriptor closes, the
// kernel may reuse the number, so the resize could be applied to an unrelated
// file. Run this with -race; without it, it only proves nothing panics.
func TestResizeAndCloseDoNotRace(t *testing.T) {
	p, err := UnixStarter{}.Start(context.Background(), weavewire.ExecRequest{
		ExecID: "race", Argv: []string{"sh", "-c", "sleep 0.2"}, TTY: true, Cols: 80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 200 {
			// The error is deliberately ignored: once Close has run, a resize
			// is expected to be refused. What must not happen is a race.
			_ = p.Resize(uint16(80+i%10), 24)
		}
	}()
	go func() {
		defer wg.Done()
		_, _, _ = p.Wait()
		_ = p.Close()
	}()
	wg.Wait()

	// Close is idempotent, and a resize after it is refused rather than applied.
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := p.Resize(100, 40); err == nil {
		t.Error(
			"a resize after close was accepted; it would touch a descriptor that may have been reused",
		)
	}
}
