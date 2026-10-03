package modulesdk

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/ipc"
)

// stubProcess swaps serve's process-level effects for the test's duration.
// sig receives the channel serve registers for signals.
func stubProcess(t *testing.T, out io.Writer) (sig chan chan<- os.Signal, codes chan int) {
	t.Helper()
	oldOut, oldNotify, oldExit := handshakeOut, notifySignals, exit
	t.Cleanup(func() { handshakeOut, notifySignals, exit = oldOut, oldNotify, oldExit })
	sig = make(chan chan<- os.Signal, 1)
	codes = make(chan int, 1)
	handshakeOut = out
	notifySignals = func(c chan<- os.Signal, _ ...os.Signal) { sig <- c }
	exit = func(code int) { codes <- code }
	return sig, codes
}

// coreEnv sets the handshake environment core would, pointing at h.
func coreEnv(t *testing.T, h *fakeHost) {
	t.Helper()
	t.Setenv(handshake.EnvProtocolMin, "1")
	t.Setenv(handshake.EnvProtocolMax, "1")
	t.Setenv(handshake.EnvToken, testToken)
	t.Setenv(handshake.EnvHostAddr, h.addr)
	t.Setenv(handshake.EnvSocketDir, shortDir(t))
}

func TestServeRefusesBeforeListening(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"bad window":    {handshake.EnvProtocolMin: "x", handshake.EnvProtocolMax: "1"},
		"out of window": {handshake.EnvProtocolMin: "2", handshake.EnvProtocolMax: "3"},
		"no token":      {handshake.EnvProtocolMin: "1", handshake.EnvProtocolMax: "1", handshake.EnvToken: "", handshake.EnvHostAddr: "h"},
		"no host addr":  {handshake.EnvProtocolMin: "1", handshake.EnvProtocolMax: "1", handshake.EnvToken: "t", handshake.EnvHostAddr: ""},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if err := serve(&fakeModule{}, discardLog()); err == nil {
				t.Fatal("serve accepted the environment")
			}
		})
	}
}

func TestServeExitCodes(t *testing.T) {
	t.Run("protocol unsupported", func(t *testing.T) {
		_, codes := stubProcess(t, io.Discard)
		t.Setenv(handshake.EnvProtocolMin, "2")
		t.Setenv(handshake.EnvProtocolMax, "3")
		Serve(&fakeModule{})
		if code := <-codes; code != handshake.ExitProtocolUnsupported {
			t.Fatalf("exit %d, want %d", code, handshake.ExitProtocolUnsupported)
		}
	})
	t.Run("other failure", func(t *testing.T) {
		_, codes := stubProcess(t, io.Discard)
		t.Setenv(handshake.EnvProtocolMin, "1")
		t.Setenv(handshake.EnvProtocolMax, "1")
		t.Setenv(handshake.EnvToken, "")
		Serve(&fakeModule{})
		if code := <-codes; code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	})
	t.Run("signal", func(t *testing.T) {
		sig, codes := stubProcess(t, io.Discard)
		coreEnv(t, newFakeHost(t))
		go func() { (<-sig) <- os.Interrupt }()
		Serve(&fakeModule{})
		if code := <-codes; code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
	})
}

// runServe starts serve in-process and returns the parsed handshake line, a
// ModuleService client on it, and serve's eventual result.
func runServe(t *testing.T, m Module) (handshake.Line, agentv1.ModuleServiceClient, <-chan error) {
	t.Helper()
	pr, pw := io.Pipe()
	stubProcess(t, pw)
	done := make(chan error, 1)
	go func() { done <- serve(m, discardLog()) }()

	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		if sc.Scan() {
			lineCh <- sc.Text()
		}
		io.Copy(io.Discard, pr) //nolint:errcheck
	}()
	var raw string
	select {
	case raw = <-lineCh:
	case err := <-done:
		t.Fatalf("serve returned before the handshake: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("no handshake line")
	}
	line, err := handshake.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := ipc.GRPCClient(line.Network, line.Addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return line, agentv1.NewModuleServiceClient(conn), done
}

func waitServe(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return")
	}
}

func TestServeEndToEnd(t *testing.T) {
	h := newFakeHost(t)
	coreEnv(t, h)
	m := &scheduledModule{}
	line, client, done := runServe(t, m)

	if line.Protocol != Protocol || line.Network != ipc.Network() {
		t.Fatalf("line = %+v", line)
	}
	if runtime.GOOS != "windows" && !strings.HasSuffix(line.Addr, "fake.sock") {
		t.Fatalf("addr = %q, want the module socket in the core-owned dir", line.Addr)
	}
	ctx := ctxT(t)
	if _, err := client.Init(ctx, initReq("fake")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Start(ctx, &agentv1.StartRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Shutdown(ctx, &agentv1.ShutdownRequest{}); err != nil {
		t.Fatal(err)
	}
	waitServe(t, done)
}

func TestServeExitsWhenCoreIsLost(t *testing.T) {
	h := newFakeHost(t)
	coreEnv(t, h)
	_, client, done := runServe(t, &fakeModule{})
	ctx := ctxT(t)
	if _, err := client.Init(ctx, initReq("fake")); err != nil {
		t.Fatal(err)
	}
	h.srv.Stop()
	waitServe(t, done)
}

func TestModuleAddr(t *testing.T) {
	if runtime.GOOS == "windows" {
		a, err := moduleAddr("toy")
		if err != nil || !strings.HasPrefix(a, `\\.\pipe\weave-toy-`) {
			t.Fatalf("moduleAddr = %q, %v", a, err)
		}
		b, _ := moduleAddr("toy")
		if a == b {
			t.Fatal("pipe names repeat")
		}
		return
	}
	t.Setenv(handshake.EnvSocketDir, "")
	if _, err := moduleAddr("toy"); err == nil {
		t.Fatal("moduleAddr without a socket dir succeeded")
	}
}
