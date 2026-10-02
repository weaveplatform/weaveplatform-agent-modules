//go:build unix

// These drive real Unix shell commands end to end. The Windows starter is a
// different implementation with its own tests in pkg/weaveexec.

package weaveclient_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveexec"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// Exec driven the way a CLI would: through weaveclient.Client, over a real pipe
// with real hvchannel framing, into the real weaveexec Manager and a real
// process. Everything except the device node is production code.

func execClient(t *testing.T) *weaveclient.Client {
	t.Helper()
	return wire(t, weaveexec.NewService(weaveexec.UnixStarter{}))
}

func TestExecRoundTripStdoutAndExitCode(t *testing.T) {
	client := execClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	session, err := client.Exec(ctx, weavewire.ExecRequest{
		Argv: []string{"sh", "-c", "echo hello from the guest; exit 5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.PID() == 0 {
		t.Error("no pid reported")
	}

	out, err := io.ReadAll(session.Stdout())
	if err != nil {
		t.Fatalf("reading stdout: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello from the guest" {
		t.Fatalf("stdout = %q", got)
	}

	code, err := session.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	// A non-zero exit is data, not an error: the command ran correctly and
	// reported failure. Conflating the two makes callers retry working commands.
	if code != 5 {
		t.Fatalf("exit code = %d, want 5", code)
	}
}

// Output far larger than one chunk must survive the trip byte-for-byte. This is
// the whole chunked design under load, through both the guest's writer and the
// host's assembler.
func TestExecRoundTripLargeOutput(t *testing.T) {
	client := execClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const lines = 5000
	session, err := client.Exec(ctx, weavewire.ExecRequest{
		Argv: []string{
			"sh",
			"-c",
			"i=0; while [ $i -lt 5000 ]; do echo 0123456789ABCDEF; i=$((i+1)); done",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := io.ReadAll(session.Stdout())
	if err != nil {
		t.Fatalf("reading stdout: %v", err)
	}
	want := strings.Repeat("0123456789ABCDEF\n", lines)
	if string(out) != want {
		t.Fatalf("got %d bytes, want %d — the stream did not reassemble", len(out), len(want))
	}
	if code, err := session.Wait(ctx); err != nil || code != 0 {
		t.Fatalf("wait = %d, %v", code, err)
	}
}

// The host writes stdin and closes it; the guest's process must see both.
func TestExecRoundTripStdin(t *testing.T) {
	client := execClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	session, err := client.Exec(ctx, weavewire.ExecRequest{
		Argv: []string{"sh", "-c", "cat; echo done"}, Stdin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(session, "piped input\n"); err != nil {
		t.Fatal(err)
	}
	if err := session.CloseStdin(); err != nil {
		t.Fatal(err)
	}

	out, err := io.ReadAll(session.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "piped input") {
		t.Fatalf("the process never saw stdin: %q", out)
	}
	if !strings.Contains(string(out), "done") {
		t.Fatalf("closing stdin did not release the process: %q", out)
	}
	if code, err := session.Wait(ctx); err != nil || code != 0 {
		t.Fatalf("wait = %d, %v", code, err)
	}
}

// stdout and stderr must arrive separately when there is no terminal to merge
// them — a caller parsing output should not have diagnostics mixed into it.
func TestExecRoundTripSeparatesStderr(t *testing.T) {
	client := execClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	session, err := client.Exec(ctx, weavewire.ExecRequest{
		Argv: []string{"sh", "-c", "echo to-stdout; echo to-stderr 1>&2"},
	})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		data string
		err  error
	}
	errCh := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(session.Stderr())
		errCh <- result{string(b), err}
	}()

	out, err := io.ReadAll(session.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	got := <-errCh
	if got.err != nil {
		t.Fatal(got.err)
	}

	if !strings.Contains(string(out), "to-stdout") || strings.Contains(string(out), "to-stderr") {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(got.data, "to-stderr") {
		t.Fatalf("stderr = %q", got.data)
	}
}

// Several execs share one channel. Their streams must not cross — the exec id
// on every chunk is what keeps them apart.
func TestConcurrentExecsDoNotCrossStreams(t *testing.T) {
	client := execClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const n = 8
	type outcome struct {
		want string
		got  string
		err  error
	}
	results := make(chan outcome, n)

	for i := range n {
		marker := strings.Repeat(string(rune('a'+i)), 64)
		go func() {
			session, err := client.Exec(ctx, weavewire.ExecRequest{
				Argv: []string{"sh", "-c", "echo " + marker},
			})
			if err != nil {
				results <- outcome{err: err}
				return
			}
			b, err := io.ReadAll(session.Stdout())
			if err == nil {
				_, err = session.Wait(ctx)
			}
			results <- outcome{want: marker, got: strings.TrimSpace(string(b)), err: err}
		}()
	}

	for range n {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.got != r.want {
			t.Fatalf("an exec received another's output: got %q, want %q", r.got, r.want)
		}
	}
}

// A terminal exec merges the streams and gives the process a real tty, which is
// what `weave ssh` depends on.
func TestExecRoundTripTTY(t *testing.T) {
	client := execClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The process must still be RUNNING when the resize arrives, so it waits on
	// stdin rather than exiting immediately. `sh -c "tty"` finishes in
	// microseconds, and the manager forgets a session the moment its process
	// exits — so on a loaded machine the resize below arrived after the exec had
	// gone and failed with "no such exec". The race was in the test, not the
	// code: a resize for a finished exec SHOULD be refused.
	session, err := client.Exec(ctx, weavewire.ExecRequest{
		Argv: []string{"sh", "-c", "tty; read _"}, TTY: true, Stdin: true, Cols: 100, Rows: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Resizing must be accepted while the process runs.
	if err := session.Resize(ctx, 120, 40); err != nil {
		t.Fatalf("resize: %v", err)
	}
	// Let it finish by satisfying the read, NOT by closing stdin: under a
	// pseudo-terminal stdin and stdout are the same file, so closing it hangs the
	// session up — the child takes SIGHUP and the output read fails with "file
	// already closed". That is what a real terminal does when it closes, so it is
	// the session's business, not this test's.
	if _, err := io.WriteString(session, "\n"); err != nil {
		t.Fatal(err)
	}

	out, _ := io.ReadAll(session.Stdout())
	if !strings.Contains(string(out), "/dev/") {
		t.Fatalf("the process had no terminal: %q", out)
	}
	if code, err := session.Wait(ctx); err != nil || code != 0 {
		t.Fatalf("wait = %d, %v", code, err)
	}
}

// A signal sent from the host must reach the process and be reported back.
func TestExecSignalFromTheHost(t *testing.T) {
	client := execClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	session, err := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := session.Signal(ctx, weavewire.SignalKill); err != nil {
		t.Fatal(err)
	}

	code, err := session.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code == 0 {
		t.Fatal("a killed process reported success")
	}
	if session.ExitSignal() == "" {
		t.Error("the exit did not name the signal that caused it")
	}
}

// A program that does not exist must fail on Exec, not surface as an exit code:
// the caller needs to tell "never ran" from "ran and failed".
func TestExecFailureToStartIsAnError(t *testing.T) {
	client := execClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.Exec(ctx, weavewire.ExecRequest{
		Argv: []string{"definitely-not-a-real-binary-xyz"},
	}); err == nil {
		t.Fatal("starting a nonexistent program reported success")
	}
}
