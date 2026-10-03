//go:build darwin

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// channel puts the module's real service behind core's channel gate and
// returns the host's client, not yet authenticated, with the key that will
// authenticate it.
func channel(t *testing.T) (*weaveclient.Client, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	core.Serve(t, newService())
	go core.Run()

	ctx, cancel := context.WithCancel(context.Background())
	client := weaveclient.New(
		ctx,
		hostConn,
		weaveclient.Options{Log: slog.New(slog.DiscardHandler)},
	)
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = core.Close()
	})
	return client, priv
}

// The host's view: real processes started through the channel, their output
// and exit codes relayed back over real framing. Exec is never open before
// authentication — it runs whatever the host names.
func TestExecThroughTheChannel(t *testing.T) {
	client, priv := channel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.Exec(
		ctx,
		weavewire.ExecRequest{Argv: []string{"true"}},
	); !errors.Is(
		err,
		weaveclient.ErrNotAuthenticated,
	) {
		t.Fatalf("pre-auth exec: err = %v, want ErrNotAuthenticated", err)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	t.Run("stdout and exit code", func(t *testing.T) {
		s, err := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"echo", "hello", "guest"}})
		if err != nil {
			t.Fatal(err)
		}
		if s.PID() == 0 {
			t.Error("no pid reported")
		}
		if out := readAll(t, s.Stdout()); out != "hello guest\n" {
			t.Errorf("stdout = %q", out)
		}
		if code, err := s.Wait(ctx); err != nil || code != 0 {
			t.Errorf("wait = %d, %v", code, err)
		}
	})

	// A non-zero exit is the command's answer, not a failed exec.
	t.Run("failure is an exit code", func(t *testing.T) {
		s, err := client.Exec(
			ctx,
			weavewire.ExecRequest{Argv: []string{"sh", "-c", "echo oops >&2; exit 3"}},
		)
		if err != nil {
			t.Fatal(err)
		}
		stdout, stderr := readAll(t, s.Stdout()), readAll(t, s.Stderr())
		if stdout != "" || stderr != "oops\n" {
			t.Errorf(
				"stdout %q, stderr %q: the streams must stay apart without a terminal",
				stdout,
				stderr,
			)
		}
		if code, err := s.Wait(ctx); err != nil || code != 3 {
			t.Errorf("wait = %d, %v; want 3", code, err)
		}
	})

	t.Run("stdin", func(t *testing.T) {
		s, err := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"cat"}, Stdin: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(s, "piped in\n"); err != nil {
			t.Fatal(err)
		}
		if err := s.CloseStdin(); err != nil {
			t.Fatal(err)
		}
		if out := readAll(t, s.Stdout()); out != "piped in\n" {
			t.Errorf("stdout = %q", out)
		}
		if code, err := s.Wait(ctx); err != nil || code != 0 {
			t.Errorf("wait = %d, %v", code, err)
		}
	})

	// Under a terminal the program sees a tty, and the terminal has the size
	// the host asked for.
	t.Run("terminal", func(t *testing.T) {
		s, err := client.Exec(ctx, weavewire.ExecRequest{
			Argv: []string{"sh", "-c", "test -t 1 && stty size"}, TTY: true, Cols: 100, Rows: 30,
		})
		if err != nil {
			t.Fatal(err)
		}
		if out := readAll(t, s.Stdout()); !strings.Contains(out, "30 100") {
			t.Errorf("terminal output = %q, want the size 30 100", out)
		}
		if code, err := s.Wait(ctx); err != nil || code != 0 {
			t.Errorf("wait = %d, %v", code, err)
		}
	})

	t.Run("signal", func(t *testing.T) {
		s, err := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"sleep", "60"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Signal(ctx, weavewire.SignalTerm); err != nil {
			t.Fatal(err)
		}
		// A signalled process reports the conventional 128+n code alongside
		// the signal, so a caller reading only the code still sees failure.
		code, err := s.Wait(ctx)
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if code != 128+15 || s.ExitSignal() == "" {
			t.Errorf("exit = %d, signal %q; want 143 and the signal named", code, s.ExitSignal())
		}
	})

	// A program that cannot start is an error on Exec, not an exit code.
	t.Run("missing program", func(t *testing.T) {
		if _, err := client.Exec(
			ctx,
			weavewire.ExecRequest{Argv: []string{"/nonexistent/weave-test"}},
		); err == nil {
			t.Fatal("exec of a missing program reported a start")
		}
	})
}

// Policy delivered by core gates exec in the module itself: a refused program
// never starts, and the refusal is audited.
func TestExecPolicyRefuses(t *testing.T) {
	h := weavemoduletest.Start(t, newService(), func(h *weavemoduletest.Host) {
		h.SetPolicy([]byte(`{"deny_argv0":["rm"]}`), nil)
	})
	res := h.Call(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "denied", Argv: []string{"rm", "-rf", "/tmp/nothing"}},
	)
	if !strings.Contains(res.Err, "refused") {
		t.Fatalf("result = %+v, want a policy refusal", res)
	}
	if len(h.Host.Published()) == 0 {
		t.Error("the refusal was not audited")
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	return string(b)
}
