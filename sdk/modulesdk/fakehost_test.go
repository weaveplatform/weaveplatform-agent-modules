package modulesdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/ipc"
)

const testToken = "test-token"

// shortDir returns a short socket directory: t.TempDir() under a long test
// name overruns the 104-byte sun_path limit on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wmsdk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func hostAddr(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		return `\\.\pipe\weave-msdk-test-` + hex.EncodeToString(b[:])
	}
	return filepath.Join(shortDir(t), "host.sock")
}

// fakeHost is an in-process stand-in for core's per-module host services.
// Unlike testkit's stub it can be told to fail, so the SDK's error paths are
// reachable.
type fakeHost struct {
	addr string
	srv  *grpc.Server

	mu       sync.Mutex
	failCode codes.Code // non-OK: every unary call fails with this code
	store    map[string][]byte
	sends    []*agentv1.TransportSendRequest
	publish  []*agentv1.PublishRequest
	badToken int

	logs  chan *agentv1.LogRecord
	pings chan *agentv1.WatchdogPing
	// notifyFails makes WatchdogService.Notify end the stream at once, so
	// the client's next Send fails.
	notifyFails bool
	// noRegistry makes RegistryService answer Unimplemented, as a core
	// from before weave-agent v0.9.2 does.
	noRegistry bool
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{
		addr:  hostAddr(t),
		store: map[string][]byte{},
		logs:  make(chan *agentv1.LogRecord, 256),
		pings: make(chan *agentv1.WatchdogPing, 256),
	}
	lis, err := ipc.Listen(h.addr)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = grpc.NewServer(
		grpc.UnaryInterceptor(
			func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
				h.checkToken(ctx)
				h.mu.Lock()
				code := h.failCode
				h.mu.Unlock()
				if code != codes.OK {
					return nil, status.Error(code, "injected")
				}
				return next(ctx, req)
			},
		),
		grpc.StreamInterceptor(
			func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
				h.checkToken(ss.Context())
				return next(srv, ss)
			},
		),
	)
	agentv1.RegisterIdentityServiceServer(h.srv, identitySrv{})
	agentv1.RegisterTransportServiceServer(h.srv, transportSrv{h: h})
	agentv1.RegisterPolicyServiceServer(h.srv, policySrv{})
	agentv1.RegisterStoreServiceServer(h.srv, storeSrv{h: h})
	agentv1.RegisterEventBusServiceServer(h.srv, eventsSrv{h: h})
	agentv1.RegisterLogServiceServer(h.srv, logSrv{h: h})
	agentv1.RegisterWatchdogServiceServer(h.srv, watchdogSrv{h: h})
	agentv1.RegisterRegistryServiceServer(h.srv, registrySrv{h: h})
	go h.srv.Serve(lis) //nolint:errcheck
	t.Cleanup(h.srv.Stop)
	return h
}

func (h *fakeHost) checkToken(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(handshake.TokenMetadataKey); len(v) != 1 || v[0] != testToken {
		h.mu.Lock()
		h.badToken++
		h.mu.Unlock()
	}
}

func (h *fakeHost) setFail(c codes.Code) {
	h.mu.Lock()
	h.failCode = c
	h.mu.Unlock()
}

// dial returns a hostClient on h, closed at cleanup.
func (h *fakeHost) dial(t *testing.T) *hostClient {
	t.Helper()
	hc, err := dialHost(ipc.Network(), h.addr, testToken, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hc.Close() })
	return hc
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type identitySrv struct {
	agentv1.UnimplementedIdentityServiceServer
}

func (identitySrv) WhoAmI(
	context.Context,
	*agentv1.WhoAmIRequest,
) (*agentv1.DeviceIdentity, error) {
	return &agentv1.DeviceIdentity{DeviceId: "dev-1", Ephemeral: true, Tenant: "acme"}, nil
}

func (identitySrv) Credential(
	_ context.Context,
	req *agentv1.CredentialRequest,
) (*agentv1.CredentialResponse, error) {
	return &agentv1.CredentialResponse{
		Token:         "tok",
		ExpiresAt:     4102444800,
		GrantedScopes: req.GetScopes(),
	}, nil
}

type transportSrv struct {
	agentv1.UnimplementedTransportServiceServer
	h *fakeHost
}

func (s transportSrv) Send(
	_ context.Context,
	req *agentv1.TransportSendRequest,
) (*agentv1.TransportSendResponse, error) {
	s.h.mu.Lock()
	s.h.sends = append(s.h.sends, req)
	s.h.mu.Unlock()
	return &agentv1.TransportSendResponse{Delivered: !req.GetQueueOffline()}, nil
}

// Receive delivers two messages and then ends the stream, which closes the
// client's channel.
func (s transportSrv) Receive(
	_ *agentv1.TransportReceiveRequest,
	stream agentv1.TransportService_ReceiveServer,
) error {
	for i, kind := range []string{"a", "b"} {
		if err := stream.Send(
			&agentv1.TransportMessage{Peer: agentv1.Peer(i + 1), Kind: kind, Data: []byte(kind)},
		); err != nil {
			return err
		}
	}
	return nil
}

type policySrv struct {
	agentv1.UnimplementedPolicyServiceServer
}

func policyAt(rev uint64) *agentv1.PolicyDocument {
	return &agentv1.PolicyDocument{
		Revision:      rev,
		Data:          []byte(`{"x":1}`),
		SchemaVersion: 2,
		ContentType:   "application/json",
	}
}

func (policySrv) Get(context.Context, *agentv1.PolicyGetRequest) (*agentv1.PolicyDocument, error) {
	return policyAt(1), nil
}

// Watch sends revisions 1 and 2, then holds the stream open until the client
// goes away.
func (policySrv) Watch(
	_ *agentv1.PolicyWatchRequest,
	stream agentv1.PolicyService_WatchServer,
) error {
	for rev := uint64(1); rev <= 2; rev++ {
		if err := stream.Send(policyAt(rev)); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}

type storeSrv struct {
	agentv1.UnimplementedStoreServiceServer
	h *fakeHost
}

func (s storeSrv) Get(
	_ context.Context,
	req *agentv1.StoreGetRequest,
) (*agentv1.StoreGetResponse, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	v, ok := s.h.store[req.GetKey()]
	return &agentv1.StoreGetResponse{Value: v, Found: ok}, nil
}

func (s storeSrv) Put(
	_ context.Context,
	req *agentv1.StorePutRequest,
) (*agentv1.StorePutResponse, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.store[req.GetKey()] = req.GetValue()
	return &agentv1.StorePutResponse{}, nil
}

func (s storeSrv) Delete(
	_ context.Context,
	req *agentv1.StoreDeleteRequest,
) (*agentv1.StoreDeleteResponse, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	delete(s.h.store, req.GetKey())
	return &agentv1.StoreDeleteResponse{}, nil
}

func (s storeSrv) List(
	_ context.Context,
	req *agentv1.StoreListRequest,
) (*agentv1.StoreListResponse, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	var keys []string
	for k := range s.h.store {
		if strings.HasPrefix(k, req.GetPrefix()) {
			keys = append(keys, k)
		}
	}
	return &agentv1.StoreListResponse{Keys: keys}, nil
}

type eventsSrv struct {
	agentv1.UnimplementedEventBusServiceServer
	h *fakeHost
}

func (s eventsSrv) Publish(
	_ context.Context,
	req *agentv1.PublishRequest,
) (*agentv1.PublishResponse, error) {
	s.h.mu.Lock()
	s.h.publish = append(s.h.publish, req)
	s.h.mu.Unlock()
	return &agentv1.PublishResponse{}, nil
}

// Subscribe echoes one event per requested topic, then ends the stream.
func (s eventsSrv) Subscribe(
	req *agentv1.SubscribeRequest,
	stream agentv1.EventBusService_SubscribeServer,
) error {
	for i, topic := range req.GetTopics() {
		if err := stream.Send(
			&agentv1.Event{
				Topic:         topic,
				Data:          []byte(topic),
				PublishedAtMs: 1000,
				Sequence:      uint64(i + 1),
			},
		); err != nil {
			return err
		}
	}
	return nil
}

type logSrv struct {
	agentv1.UnimplementedLogServiceServer
	h *fakeHost
}

func (s logSrv) Write(
	stream grpc.ClientStreamingServer[agentv1.LogRecord, agentv1.LogWriteResponse],
) error {
	for {
		rec, err := stream.Recv()
		if err != nil {
			return stream.SendAndClose(&agentv1.LogWriteResponse{})
		}
		s.h.logs <- rec
	}
}

type watchdogSrv struct {
	agentv1.UnimplementedWatchdogServiceServer
	h *fakeHost
}

func (s watchdogSrv) Notify(
	stream grpc.ClientStreamingServer[agentv1.WatchdogPing, agentv1.WatchdogSummary],
) error {
	s.h.mu.Lock()
	fail := s.h.notifyFails
	s.h.mu.Unlock()
	if fail {
		return status.Error(codes.Unavailable, "injected")
	}
	for {
		p, err := stream.Recv()
		if err != nil {
			return stream.SendAndClose(&agentv1.WatchdogSummary{})
		}
		select {
		case s.h.pings <- p:
		default:
		}
	}
}

// locked runs f under the host's lock, for tests reading what the server
// goroutines recorded.
func (h *fakeHost) locked(f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f()
}

type registrySrv struct {
	agentv1.UnimplementedRegistryServiceServer
	h *fakeHost
}

func registryAt(rev uint64) *agentv1.RegistrySnapshot {
	return &agentv1.RegistrySnapshot{Revision: rev, Modules: []*agentv1.RegisteredModule{
		{
			Id: "weave-linux-clipboard", Version: "0.4.0", Address: "weave.clipboard",
			State: "waiting-for-session",
		},
		{
			Id: "weave-linux-power", Version: "1.2.3", Address: "weave.power", State: "running",
			Health: &agentv1.Health{
				Status:  agentv1.Health_STATUS_DEGRADED,
				Reason:  "slow",
				Details: map[string]string{"a": "b"},
			},
		},
	}}
}

func (s registrySrv) unimplemented() bool {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.h.noRegistry
}

func (s registrySrv) List(
	context.Context,
	*agentv1.RegistryListRequest,
) (*agentv1.RegistrySnapshot, error) {
	if s.unimplemented() {
		return nil, status.Error(codes.Unimplemented, "unknown service")
	}
	return registryAt(3), nil
}

// Watch sends revisions 1 and 2, then holds the stream open until the client
// goes away.
func (s registrySrv) Watch(
	_ *agentv1.RegistryWatchRequest,
	stream agentv1.RegistryService_WatchServer,
) error {
	if s.unimplemented() {
		return status.Error(codes.Unimplemented, "unknown service")
	}
	for rev := uint64(1); rev <= 2; rev++ {
		if err := stream.Send(registryAt(rev)); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}
