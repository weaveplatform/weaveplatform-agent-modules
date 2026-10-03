package weaveclient_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/hvchannel"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveexec"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemetrics"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavepower"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavetime"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// rawGuest is the guest end with no module behind it: the test reads exactly
// what the host wrote and answers by hand, to pin the bytes on the wire.
type rawGuest struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
}

func newRaw(t *testing.T) (*rawGuest, *weaveclient.Client) {
	t.Helper()
	guestConn, hostConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	client := weaveclient.New(ctx, hostConn, weaveclient.Options{Log: quietLog()})
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		_ = guestConn.Close()
	})
	return &rawGuest{
		t:    t,
		conn: guestConn,
		r:    bufio.NewReader(guestConn),
		w:    bufio.NewWriter(guestConn),
	}, client
}

func (g *rawGuest) read() hvchannel.Envelope {
	g.t.Helper()
	env, err := hvchannel.ReadEnvelope(g.r)
	if err != nil {
		g.t.Errorf("guest read: %v", err)
	}
	return env
}

func (g *rawGuest) write(env hvchannel.Envelope) {
	g.t.Helper()
	if err := hvchannel.WriteEnvelope(g.w, env); err != nil {
		g.t.Errorf("guest write: %v", err)
		return
	}
	if err := g.w.Flush(); err != nil {
		g.t.Errorf("guest flush: %v", err)
	}
}

// reply answers cmd from the address its kind belongs to.
func (g *rawGuest) reply(cmd hvchannel.Envelope, res weavewire.Result) {
	g.t.Helper()
	var in weavewire.Command
	if err := json.Unmarshal(cmd.Data, &in); err != nil {
		g.t.Errorf("decoding command: %v", err)
	}
	res.ID = in.ID
	data, _ := json.Marshal(res)
	g.write(
		hvchannel.Envelope{Module: cmd.Module, Kind: weavewire.ResultKind(cmd.Kind), Data: data},
	)
}

// Every typed call goes to its capability's address, never to an OS-specific
// module and never to the old single "guestweave" module. This is the whole
// of capability addressing from the host's side.
func TestEachCallIsAddressedToItsCapability(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cases := []struct {
		call    func() error
		module  string
		kind    string
		payload string
	}{
		{
			func() error { _, err := client.Hello(ctx); return err },
			"weave.presence",
			weavewire.KindPresenceHello,
			`{}`,
		},
		{
			func() error { _, err := client.Inventory(ctx); return err },
			"weave.presence",
			weavewire.KindPresenceInventory,
			`{}`,
		},
		{
			func() error { _, err := client.Shutdown(ctx, "r"); return err },
			"weave.power",
			weavewire.KindPowerShutdown,
			`{"accepted":true}`,
		},
		{
			func() error { _, err := client.Restart(ctx, "r"); return err },
			"weave.power",
			weavewire.KindPowerRestart,
			`{"accepted":true}`,
		},
		{
			func() error { _, err := client.Time(ctx); return err },
			"weave.time",
			weavewire.KindTimeGet,
			`{}`,
		},
		{
			func() error { _, err := client.SetTime(ctx, time.Now(), time.Minute); return err },
			"weave.time",
			weavewire.KindTimeSet,
			`{}`,
		},
		{
			func() error { _, err := client.Metrics(ctx); return err },
			"weave.metrics",
			weavewire.KindMetricsSample,
			`{}`,
		},
		{
			func() error { _, err := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"x"}}); return err },
			"weave.exec",
			weavewire.KindExecStart,
			`{}`,
		},
	}
	for _, tc := range cases {
		errc := make(chan error, 1)
		go func() { errc <- tc.call() }()
		env := guest.read()
		if env.Module != tc.module || env.Kind != tc.kind {
			t.Fatalf("sent %s/%s, want %s/%s", env.Module, env.Kind, tc.module, tc.kind)
		}
		guest.reply(env, weavewire.Result{Payload: json.RawMessage(tc.payload)})
		if err := <-errc; err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
	}
}

// A reply must come back from the address its kind belongs to. One that
// arrives under another address is dropped: core stamps the sender's address,
// so a mismatch means a module answering for a capability it does not own.
func TestRepliesFromTheWrongAddressAreIgnored(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	errc := make(chan error, 1)
	go func() { _, err := client.Hello(ctx); errc <- err }()
	env := guest.read()
	env.Module = "weave.exec"
	guest.reply(env, weavewire.Result{Payload: json.RawMessage(`{}`)})
	if err := <-errc; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the misaddressed reply ignored", err)
	}
}

func TestUnsupportedIsDistinguishable(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() { _, err := client.SetTime(ctx, time.Now(), 0); errc <- err }()
	guest.reply(guest.read(), weavewire.Result{Err: "not here", Code: weavewire.CodeUnsupported})
	err := <-errc
	if !errors.Is(err, weaveclient.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	var ge *weaveclient.GuestError
	if !errors.As(err, &ge) || ge.Code != weavewire.CodeUnsupported || ge.Error() == "" {
		t.Fatalf("err = %#v", err)
	}

	// An ordinary refusal is a GuestError but not "unsupported".
	go func() { _, err := client.SetTime(ctx, time.Now(), 0); errc <- err }()
	guest.reply(guest.read(), weavewire.Result{Err: "refusing"})
	if err := <-errc; errors.Is(err, weaveclient.ErrUnsupported) || !errors.As(err, &ge) {
		t.Fatalf("err = %v", err)
	}
}

func TestUndecodableRepliesAndNoiseDoNotBreakTheChannel(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var events int
	got := make(chan struct{}, 1)
	client.On("weave.time.drift", func(string, []byte) { events++; got <- struct{}{} })

	errc := make(chan error, 1)
	go func() { _, err := client.Time(ctx); errc <- err }()
	cmd := guest.read()

	// Garbage inside a frame, a result nobody waits for, an undecodable
	// result, an event nobody handles, a frame for another namespace, and a
	// stray control frame: none of them may end the channel.
	if err := hvchannel.WriteFrame(guest.w, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	guest.write(
		hvchannel.Envelope{
			Module: "weave.time",
			Kind:   weavewire.ResultKind(weavewire.KindTimeGet),
			Data:   []byte(`{"id":"nobody"}`),
		},
	)
	guest.write(
		hvchannel.Envelope{
			Module: "weave.time",
			Kind:   weavewire.ResultKind(weavewire.KindTimeGet),
			Data:   []byte(`[]`),
		},
	)
	guest.write(hvchannel.Envelope{Module: "weave.time", Kind: "weave.time.unheard"})
	guest.write(hvchannel.Envelope{Module: "example", Kind: "example.report"})
	guest.write(
		hvchannel.Envelope{Module: hvchannel.ControlModule, Kind: hvchannel.KindAuthChallenge},
	)
	guest.write(
		hvchannel.Envelope{
			Module: hvchannel.ControlModule,
			Kind:   hvchannel.KindAuthResult,
			Data:   []byte(`{"ok":true}`),
		},
	)
	guest.write(
		hvchannel.Envelope{
			Module: "weave.time",
			Kind:   "weave.time.drift",
			Data:   []byte(`{}`),
		},
	)
	<-got

	// A payload that does not decode into the typed response is an error
	// for that call alone.
	guest.reply(cmd, weavewire.Result{Payload: json.RawMessage(`"a string"`)})
	if err := <-errc; err == nil {
		t.Fatal("a mistyped payload decoded")
	}
	if client.Err() != nil || events != 1 {
		t.Fatalf("channel err = %v, events = %d", client.Err(), events)
	}
}

func TestBadKindsAreRefusedLocally(t *testing.T) {
	_, client := newRaw(t)
	for _, kind := range []string{"example.collect", "weave.", "weave.time", weavewire.ResultKind(weavewire.KindTimeGet)} {
		if err := client.Call(
			context.Background(),
			kind,
			nil,
			nil,
		); !errors.Is(
			err,
			weaveclient.ErrBadKind,
		) {
			t.Errorf("Call(%q) = %v, want ErrBadKind", kind, err)
		}
		if err := client.Notify(kind, nil); !errors.Is(err, weaveclient.ErrBadKind) {
			t.Errorf("Notify(%q) = %v, want ErrBadKind", kind, err)
		}
	}
	if err := client.Call(context.Background(), weavewire.KindTimeGet, func() {}, nil); err == nil {
		t.Error("an unencodable payload was sent")
	}
	if err := client.Notify(weavewire.KindExecStdin, func() {}); err == nil {
		t.Error("an unencodable notification was sent")
	}
}

// Cancelling the client's context must end it even while the read loop is
// blocked in Read, where a context cannot reach.
func TestCancellingTheContextClosesTheChannel(t *testing.T) {
	_, hostConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	client := weaveclient.New(ctx, hostConn, weaveclient.Options{})
	cancel()
	select {
	case <-client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the client outlived its context")
	}
	if !errors.Is(client.Err(), context.Canceled) {
		t.Fatalf("err = %v", client.Err())
	}
	if err := client.Notify(weavewire.KindExecStdin, nil); err == nil {
		t.Fatal("a closed client sent")
	}
	if err := client.Authenticate(
		context.Background(),
		make(ed25519.PrivateKey, ed25519.PrivateKeySize),
	); err == nil {
		t.Fatal("a closed client authenticated")
	}
}

func TestCallHonoursItsContext(t *testing.T) {
	guest, client := newRaw(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := client.Metrics(ctx); errc <- err }()
	guest.read()
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// The handshake must not hang on a guest that answers out of turn or not at
// all.
func TestAuthenticateFailureModes(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	t.Run("undecodable challenge", func(t *testing.T) {
		guest, client := newRaw(t)
		go func() {
			guest.read()
			guest.write(
				hvchannel.Envelope{
					Module: hvchannel.ControlModule,
					Kind:   hvchannel.KindAuthChallenge,
					Data:   []byte(`[]`),
				},
			)
		}()
		if err := client.Authenticate(context.Background(), priv); err == nil {
			t.Fatal("accepted an undecodable challenge")
		}
	})
	t.Run("short nonce", func(t *testing.T) {
		guest, client := newRaw(t)
		go func() {
			guest.read()
			guest.write(
				hvchannel.Envelope{
					Module: hvchannel.ControlModule,
					Kind:   hvchannel.KindAuthChallenge,
					Data:   []byte(`{"nonce":"AAAA"}`),
				},
			)
		}()
		if err := client.Authenticate(context.Background(), priv); err == nil {
			t.Fatal("signed a short nonce")
		}
	})
	t.Run("undecodable result", func(t *testing.T) {
		guest, client := newRaw(t)
		go func() {
			guest.read()
			nonce, _ := hvchannel.NewNonce()
			data, _ := json.Marshal(hvchannel.AuthChallenge{Nonce: nonce})
			// An out-of-turn frame first: the handshake skips it.
			guest.write(
				hvchannel.Envelope{Module: hvchannel.ControlModule, Kind: "auth.unexpected"},
			)
			guest.write(
				hvchannel.Envelope{
					Module: hvchannel.ControlModule,
					Kind:   hvchannel.KindAuthChallenge,
					Data:   data,
				},
			)
			guest.read()
			guest.write(
				hvchannel.Envelope{
					Module: hvchannel.ControlModule,
					Kind:   hvchannel.KindAuthResult,
					Data:   []byte(`[]`),
				},
			)
		}()
		if err := client.Authenticate(context.Background(), priv); err == nil {
			t.Fatal("accepted an undecodable result")
		}
	})
	t.Run("refused at begin", func(t *testing.T) {
		guest, client := newRaw(t)
		go func() {
			guest.read()
			guest.write(
				hvchannel.Envelope{
					Module: hvchannel.ControlModule,
					Kind:   hvchannel.KindAuthResult,
					Data:   []byte(`{"reason":"no key"}`),
				},
			)
		}()
		if err := client.Authenticate(context.Background(), priv); err == nil {
			t.Fatal("a refusal at begin was waited out")
		}
	})
	t.Run("no answer", func(t *testing.T) {
		guest, client := newRaw(t)
		go guest.read()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := client.Authenticate(ctx, priv); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("channel dies", func(t *testing.T) {
		guest, client := newRaw(t)
		go func() {
			guest.read()
			_ = guest.conn.Close()
		}()
		if err := client.Authenticate(context.Background(), priv); err == nil {
			t.Fatal("authenticated over a dead channel")
		}
	})
}

// A burst of control frames during a handshake must not stall the read loop.
func TestControlFloodDuringAHandshakeIsDropped(t *testing.T) {
	guest, client := newRaw(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go func() {
		guest.read()
		for range 32 {
			guest.write(hvchannel.Envelope{Module: hvchannel.ControlModule, Kind: "auth.noise"})
		}
	}()
	_ = client.Authenticate(ctx, priv)
	if client.Err() != nil {
		t.Fatalf("the flood ended the channel: %v", client.Err())
	}
}

func TestDialAuthenticatesWhenGivenAKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := func(trusted ed25519.PublicKey) weaveclient.Dialer {
		return weaveclient.DialerFunc(func(context.Context) (io.ReadWriteCloser, error) {
			guestConn, hostConn := net.Pipe()
			core := weavemoduletest.NewCore(guestConn, trusted)
			core.Serve(t, weavemetrics.NewService(stubMetrics{}))
			go core.Run()
			t.Cleanup(func() { _ = core.Close() })
			return hostConn, nil
		})
	}

	client, err := weaveclient.Dial(ctx, dialer(pub), priv, weaveclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Metrics(ctx); err != nil {
		t.Fatalf("metrics after Dial: %v", err)
	}
	_ = client.Close()

	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := weaveclient.Dial(ctx, dialer(pub), wrong, weaveclient.Options{}); err == nil {
		t.Fatal("Dial returned a client the guest refused")
	}

	plain, err := weaveclient.Dial(ctx, dialer(nil), nil, weaveclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = plain.Close()

	failing := weaveclient.DialerFunc(func(context.Context) (io.ReadWriteCloser, error) {
		return nil, errors.New("no such VM")
	})
	if _, err := weaveclient.Dial(ctx, failing, nil, weaveclient.Options{}); err == nil {
		t.Fatal("a failed dial produced a client")
	}
}

// A VZ console port is two pipes, one per direction. Pipes makes them one
// channel the client can own.
func TestPipesCarryAChannelInBothDirections(t *testing.T) {
	hostR, guestW := io.Pipe()
	guestR, hostW := io.Pipe()
	core := weavemoduletest.NewCore(weaveclient.Pipes(guestR, guestW), nil)
	core.Serve(t, weavetime.NewService(nopClock{}))
	go core.Run()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := weaveclient.New(ctx, weaveclient.Pipes(hostR, hostW), weaveclient.Options{})
	if _, err := client.Time(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	_ = core.Close()
}

// Every capability with a wire contract served at once, as a guest with the
// whole module set installed would: each call reaches its own module.
func TestAWholeGuestAnswersEveryCapability(t *testing.T) {
	ran := make(chan string, 2)
	client := wire(t,
		weavepower.NewService(stubPower{ran: ran}),
		weavetime.NewService(nopClock{}),
		weavemetrics.NewService(stubMetrics{}),
		weaveexec.NewService(failingStarter{}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	if accepted, err := client.Restart(ctx, "test"); err != nil || !accepted {
		t.Fatalf("restart = %v, %v", accepted, err)
	}
	if <-ran != "restart" {
		t.Fatal("power did not act")
	}
	if _, err := client.SetTime(ctx, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if tm, err := client.Time(ctx); err != nil || tm.UnixNano == 0 {
		t.Fatalf("time = %+v, %v", tm, err)
	}
	if m, err := client.Metrics(ctx); err != nil || m.CPUPercent != 1 {
		t.Fatalf("metrics = %+v, %v", m, err)
	}
	if _, err := client.Exec(ctx, weavewire.ExecRequest{Argv: []string{"x"}}); err == nil {
		t.Fatal("a failing starter started")
	}
	var _ weavemodule.Service = weaveexec.NewService(failingStarter{})
}

type nopClock struct{}

func (nopClock) SetTime(context.Context, time.Time) error { return nil }

type stubMetrics struct{}

func (stubMetrics) Sample(_ context.Context, out *weavewire.MetricsResponse) error {
	out.CPUPercent = 1
	return nil
}

type failingStarter struct{}

func (failingStarter) Start(context.Context, weavewire.ExecRequest) (weaveexec.Process, error) {
	return nil, errors.New("no processes here")
}
