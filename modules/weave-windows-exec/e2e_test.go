//go:build windows

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
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

// cmdExe is named by its full path: the exec runs argv directly, never
// through a shell, so nothing resolves "cmd" for it.
func cmdExe() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return root + `\System32\cmd.exe`
}

// The host's view: real processes started through the channel, their output
// and exit codes relayed back over real framing. Exec is never open before
// authentication — it runs whatever the host names.
func TestExecThroughTheChannel(t *testing.T) {
	client, priv := channel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := client.Exec(
		ctx,
		weavewire.ExecRequest{Argv: []string{cmdExe(), "/c", "exit 0"}},
	); !errors.Is(
		err,
		weaveclient.ErrNotAuthenticated,
	) {
		t.Fatalf("pre-auth exec: err = %v, want ErrNotAuthenticated", err)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	// A non-zero exit is the command's answer, not a failed exec, and the two
	// streams stay apart without a terminal.
	t.Run("output and exit code", func(t *testing.T) {
		s, err := client.Exec(ctx, weavewire.ExecRequest{
			Argv: []string{cmdExe(), "/c", "echo out& echo err 1>&2& exit 3"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if s.PID() == 0 {
			t.Error("no pid reported")
		}
		stdout, stderr := readAll(t, s.Stdout()), readAll(t, s.Stderr())
		if !strings.Contains(stdout, "out") || strings.Contains(stdout, "err") ||
			!strings.Contains(stderr, "err") {
			t.Errorf("stdout %q, stderr %q", stdout, stderr)
		}
		if code, err := s.Wait(ctx); err != nil || code != 3 {
			t.Errorf("wait = %d, %v; want 3", code, err)
		}
	})

	t.Run("stdin", func(t *testing.T) {
		s, err := client.Exec(
			ctx,
			weavewire.ExecRequest{Argv: []string{cmdExe(), "/c", "findstr x"}, Stdin: true},
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(s, "has x\r\nnope\r\n"); err != nil {
			t.Fatal(err)
		}
		if err := s.CloseStdin(); err != nil {
			t.Fatal(err)
		}
		if out := readAll(
			t,
			s.Stdout(),
		); !strings.Contains(out, "has x") ||
			strings.Contains(out, "nope") {
			t.Errorf("stdout = %q", out)
		}
		if code, err := s.Wait(ctx); err != nil || code != 0 {
			t.Errorf("wait = %d, %v", code, err)
		}
	})

	// Under a pseudo-console the output is one merged terminal stream.
	t.Run("terminal", func(t *testing.T) {
		s, err := client.Exec(ctx, weavewire.ExecRequest{
			Argv: []string{cmdExe(), "/c", "echo MARKER& exit 7"}, TTY: true, Cols: 100, Rows: 30,
		})
		if err != nil && strings.Contains(err.Error(), "CreatePseudoConsole") {
			t.Skipf("no pseudo-console on this runner: %v", err)
		}
		if err != nil {
			t.Fatal(err)
		}
		if out := readAll(t, s.Stdout()); !strings.Contains(out, "MARKER") {
			t.Errorf("terminal output = %q", out)
		}
		if code, err := s.Wait(ctx); err != nil || code != 7 {
			t.Errorf("wait = %d, %v; want 7", code, err)
		}
	})

	// Windows has no signals: TERM is a hard termination, and INT is refused
	// rather than pretended.
	t.Run("signal", func(t *testing.T) {
		s, err := client.Exec(ctx, weavewire.ExecRequest{
			Argv: []string{cmdExe(), "/c", "ping -n 60 127.0.0.1 >NUL 2>NUL"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Signal(ctx, weavewire.SignalInt); err == nil {
			t.Error("INT was pretended rather than refused")
		}
		if err := s.Signal(ctx, weavewire.SignalTerm); err != nil {
			t.Fatal(err)
		}
		if code, err := s.Wait(ctx); err != nil || code == 0 {
			t.Errorf("wait = %d, %v; a terminated process must not report success", code, err)
		}
	})

	// A program that cannot start is an error on Exec, not an exit code.
	t.Run("missing program", func(t *testing.T) {
		if _, err := client.Exec(
			ctx,
			weavewire.ExecRequest{Argv: []string{`C:\nonexistent\weave-test.exe`}},
		); err == nil {
			t.Fatal("exec of a missing program reported a start")
		}
	})
}

// Policy delivered by core gates exec in the module itself: a refused program
// never starts, and the refusal is audited.
func TestExecPolicyRefuses(t *testing.T) {
	h := weavemoduletest.Start(t, newService(), func(h *weavemoduletest.Host) {
		h.SetPolicy([]byte(`{"allow_tty":false}`), nil)
	})
	res := h.Call(
		weavewire.KindExecStart,
		weavewire.ExecRequest{ExecID: "denied", Argv: []string{cmdExe()}, TTY: true},
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
