package testkit

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/ipc"
)

// serveHost starts the stub host services on a fresh local address and
// returns a connection to them.
func serveHost(t *testing.T, data *HostData) *grpc.ClientConn {
	t.Helper()
	addr := `\\.\pipe\weave-testkit-hostserver-` + t.Name()
	if runtime.GOOS != "windows" {
		// Short dir: t.TempDir() can overrun macOS's 104-byte sun_path.
		dir, err := os.MkdirTemp("", "wtk")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		addr = filepath.Join(dir, "h.sock")
	}
	lis, err := ipc.Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(tokenInterceptors("tok")...)
	registerHostServices(srv, data, "toy")
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)
	conn, err := ipc.GRPCClient(ipc.Network(), addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func withToken(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return metadata.AppendToOutgoingContext(ctx, handshake.TokenMetadataKey, "tok")
}

func TestHostServicesRejectMissingToken(t *testing.T) {
	conn := serveHost(t, NewHostData())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := agentv1.NewIdentityServiceClient(conn).WhoAmI(ctx, &agentv1.WhoAmIRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unary without token: %v", err)
	}
	stream, err := agentv1.NewPolicyServiceClient(conn).Watch(ctx, &agentv1.PolicyWatchRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("stream without token: %v", err)
	}
}

func TestHostServices(t *testing.T) {
	data := NewHostData()
	conn := serveHost(t, data)
	ctx := withToken(t)

	id := agentv1.NewIdentityServiceClient(conn)
	who, err := id.WhoAmI(ctx, &agentv1.WhoAmIRequest{})
	if err != nil || who.GetDeviceId() != "test-device" || who.GetTenant() != "test" {
		t.Fatalf("WhoAmI = %v, %v", who, err)
	}
	cred, err := id.Credential(ctx, &agentv1.CredentialRequest{Scopes: []string{"s"}})
	if err != nil || cred.GetToken() != "test-credential-toy" ||
		!slices.Equal(cred.GetGrantedScopes(), []string{"s"}) {
		t.Fatalf("Credential = %v, %v", cred, err)
	}

	st := agentv1.NewStoreServiceClient(conn)
	if _, err := st.Put(ctx, &agentv1.StorePutRequest{Key: "k", Value: []byte("v")}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, &agentv1.StoreGetRequest{Key: "k"})
	if err != nil || !got.GetFound() || string(got.GetValue()) != "v" {
		t.Fatalf("Get = %v, %v", got, err)
	}
	keys, err := st.List(ctx, &agentv1.StoreListRequest{Prefix: "k"})
	if err != nil || !slices.Equal(keys.GetKeys(), []string{"k"}) {
		t.Fatalf("List = %v, %v", keys, err)
	}
	if _, err := st.Delete(ctx, &agentv1.StoreDeleteRequest{Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := data.StoreGet("k"); ok {
		t.Fatal("Delete did not reach HostData")
	}

	pol := agentv1.NewPolicyServiceClient(conn)
	data.SetPolicy([]byte("p1"))
	doc, err := pol.Get(ctx, &agentv1.PolicyGetRequest{})
	if err != nil || doc.GetRevision() != 1 || string(doc.GetData()) != "p1" {
		t.Fatalf("policy Get = %v, %v", doc, err)
	}
	wctx, wcancel := context.WithCancel(ctx)
	watch, err := pol.Watch(wctx, &agentv1.PolicyWatchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := watch.Recv(); err != nil || d.GetRevision() != 1 {
		t.Fatalf("first watched = %v, %v", d, err)
	}
	data.SetPolicy([]byte("p2"))
	if d, err := watch.Recv(); err != nil || string(d.GetData()) != "p2" {
		t.Fatalf("second watched = %v, %v", d, err)
	}
	wcancel()

	ev := agentv1.NewEventBusServiceClient(conn)
	sctx, scancel := context.WithCancel(ctx)
	sub, err := ev.Subscribe(sctx, &agentv1.SubscribeRequest{Topics: []string{"toy.*"}})
	if err != nil {
		t.Fatal(err)
	}
	// Publish until the subscription is registered server-side; earlier
	// publishes have no subscriber to reach.
	recv := make(chan *agentv1.Event, 1)
	go func() {
		if e, err := sub.Recv(); err == nil {
			recv <- e
		}
	}()
	var e *agentv1.Event
	for e == nil {
		if _, err := ev.Publish(
			ctx,
			&agentv1.PublishRequest{Topic: "started", Data: []byte("d")},
		); err != nil {
			t.Fatal(err)
		}
		select {
		case e = <-recv:
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("event never delivered")
		}
	}
	if e.GetTopic() != "toy.started" || string(e.GetData()) != "d" {
		t.Fatalf("event = %v", e)
	}
	scancel()

	tr := agentv1.NewTransportServiceClient(conn)
	sent, err := tr.Send(ctx, &agentv1.TransportSendRequest{
		Message: &agentv1.TransportMessage{
			Peer: agentv1.Peer_PEER_HYPERVISOR,
			Kind: "hb",
			Data: []byte("x"),
		},
		QueueOffline: true,
	})
	if err != nil || !sent.GetDelivered() {
		t.Fatalf("Send = %v, %v", sent, err)
	}
	if s := data.Sends(); len(s) != 1 || s[0].Kind != "hb" || !s[0].QueueOffline ||
		s[0].Peer != int32(agentv1.Peer_PEER_HYPERVISOR) {
		t.Fatalf("sends = %+v", s)
	}
	rctx, rcancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer rcancel()
	rs, err := tr.Receive(rctx, &agentv1.TransportReceiveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// The stub delivers nothing inbound: the stream only ends with the
	// caller's context.
	if _, err := rs.Recv(); err == nil || err == io.EOF {
		t.Fatalf("Receive yielded %v, want the context's error", err)
	}
}

func TestRegistryService(t *testing.T) {
	data := NewHostData()
	conn := serveHost(t, data)
	ctx := withToken(t)
	reg := agentv1.NewRegistryServiceClient(conn)

	data.SetRegistryModule(RegistryEntry{ID: "weave-linux-power", Address: "weave.power"})
	data.SetRegistryModule(RegistryEntry{
		ID: "weave-linux-clipboard", Address: "weave.clipboard", State: "waiting-for-session",
		Health: agentv1.Health_STATUS_DEGRADED, HealthReason: "why",
	})
	data.SetRegistryModule(RegistryEntry{ID: "toy"})
	snap, err := reg.List(ctx, &agentv1.RegistryListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	mods := snap.GetModules()
	if snap.GetRevision() != 3 || len(mods) != 3 || mods[0].GetId() != "toy" ||
		mods[0].GetAddress() != "toy" || mods[0].GetState() != "running" || mods[0].GetHealth() != nil ||
		mods[1].GetId() != "weave-linux-clipboard" ||
		mods[1].GetHealth().GetStatus() != agentv1.Health_STATUS_DEGRADED ||
		mods[1].GetHealth().GetReason() != "why" {
		t.Fatalf("List = %v", snap)
	}

	wctx, cancel := context.WithCancel(ctx)
	stream, err := reg.Watch(wctx, &agentv1.RegistryWatchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := stream.Recv(); err != nil || first.GetRevision() != 3 {
		t.Fatalf("first watched = %v, %v", first, err)
	}
	data.RemoveRegistryModule("toy")
	if next, err := stream.Recv(); err != nil || next.GetRevision() != 4 ||
		len(next.GetModules()) != 2 {
		t.Fatalf("after remove = %v, %v", next, err)
	}
	cancel()

	data.SetRegistryUnsupported(true)
	if _, err := reg.List(
		ctx,
		&agentv1.RegistryListRequest{},
	); status.Code(
		err,
	) != codes.Unimplemented {
		t.Fatalf("unsupported List = %v", err)
	}
	stream, err = reg.Watch(ctx, &agentv1.RegistryWatchRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unsupported Watch = %v", err)
	}
}
