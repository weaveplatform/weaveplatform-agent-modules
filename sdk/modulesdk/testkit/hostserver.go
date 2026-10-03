package testkit

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
)

// The host services are one struct per service (Store.Get and Policy.Get
// share a name with different signatures, so one struct cannot serve both),
// all backed by the same HostData.

func registerHostServices(s *grpc.Server, data *HostData, moduleID string) {
	agentv1.RegisterIdentityServiceServer(s, &identityServer{data: data, moduleID: moduleID})
	agentv1.RegisterTransportServiceServer(s, &transportServer{data: data})
	agentv1.RegisterPolicyServiceServer(s, &policyServer{data: data})
	agentv1.RegisterStoreServiceServer(s, &storeServer{data: data})
	agentv1.RegisterEventBusServiceServer(s, &eventsServer{data: data, moduleID: moduleID})
}

// tokenInterceptors reject any call not presenting the expected token —
// the way real core binds a connection to a module.
func tokenInterceptors(token string) []grpc.ServerOption {
	g := tokenGate(token)
	return []grpc.ServerOption{
		grpc.UnaryInterceptor(g.unary),
		grpc.StreamInterceptor(g.stream),
	}
}

// tokenGate is the handshake token every host call must carry.
type tokenGate string

func (g tokenGate) check(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(handshake.TokenMetadataKey)
	if len(vals) != 1 || vals[0] != string(g) {
		return status.Errorf(codes.Unauthenticated, "missing or wrong handshake token")
	}
	return nil
}

func (g tokenGate) unary(
	ctx context.Context,
	req any,
	_ *grpc.UnaryServerInfo,
	h grpc.UnaryHandler,
) (any, error) {
	if err := g.check(ctx); err != nil {
		return nil, err
	}
	return h(ctx, req)
}

// stream gates a streaming call on the context the stream carries.
func (g tokenGate) stream(
	srv any,
	ss grpc.ServerStream,
	_ *grpc.StreamServerInfo,
	h grpc.StreamHandler,
) error {
	if err := g.check(ss.Context()); err != nil {
		return err
	}
	return h(srv, ss)
}

// --- Identity ---

type identityServer struct {
	agentv1.UnimplementedIdentityServiceServer
	data     *HostData
	moduleID string
}

func (s *identityServer) WhoAmI(
	ctx context.Context,
	_ *agentv1.WhoAmIRequest,
) (*agentv1.DeviceIdentity, error) {
	return &agentv1.DeviceIdentity{
		DeviceId:  s.data.Device.DeviceID,
		Ephemeral: s.data.Device.Ephemeral,
		Tenant:    s.data.Device.Tenant,
	}, nil
}

func (s *identityServer) Credential(
	ctx context.Context,
	req *agentv1.CredentialRequest,
) (*agentv1.CredentialResponse, error) {
	return &agentv1.CredentialResponse{
		Token:         "test-credential-" + s.moduleID,
		ExpiresAt:     4102444800, // 2100-01-01: never expires in a test's lifetime
		GrantedScopes: req.GetScopes(),
	}, nil
}

// --- Transport ---

type transportServer struct {
	agentv1.UnimplementedTransportServiceServer
	data *HostData
}

func (s *transportServer) Send(
	ctx context.Context,
	req *agentv1.TransportSendRequest,
) (*agentv1.TransportSendResponse, error) {
	m := req.GetMessage()
	s.data.RecordSend(SentMessage{
		Peer:         int32(m.GetPeer()),
		Kind:         m.GetKind(),
		Data:         m.GetData(),
		QueueOffline: req.GetQueueOffline(),
	})
	return &agentv1.TransportSendResponse{Delivered: true}, nil
}

func (s *transportServer) Receive(
	_ *agentv1.TransportReceiveRequest,
	stream agentv1.TransportService_ReceiveServer,
) error {
	// The stub delivers nothing inbound; block until the module hangs up.
	<-stream.Context().Done()
	return nil
}

// --- Policy ---

type policyServer struct {
	agentv1.UnimplementedPolicyServiceServer
	data *HostData
}

func (s *policyServer) Get(
	ctx context.Context,
	_ *agentv1.PolicyGetRequest,
) (*agentv1.PolicyDocument, error) {
	rev, data := s.data.Policy()
	return &agentv1.PolicyDocument{Revision: rev, Data: data}, nil
}

func (s *policyServer) Watch(
	_ *agentv1.PolicyWatchRequest,
	stream agentv1.PolicyService_WatchServer,
) error {
	notify := s.data.addPolicyWatcher()
	var sent uint64
	for {
		rev, data := s.data.Policy()
		if rev >= sent {
			if err := stream.Send(&agentv1.PolicyDocument{Revision: rev, Data: data}); err != nil {
				return fmt.Errorf("testkit: sending policy: %w", err)
			}
			sent = rev + 1
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-notify:
		}
	}
}

// --- Store ---

type storeServer struct {
	agentv1.UnimplementedStoreServiceServer
	data *HostData
}

func (s *storeServer) Get(
	ctx context.Context,
	req *agentv1.StoreGetRequest,
) (*agentv1.StoreGetResponse, error) {
	v, ok := s.data.StoreGet(req.GetKey())
	return &agentv1.StoreGetResponse{Value: v, Found: ok}, nil
}

func (s *storeServer) Put(
	ctx context.Context,
	req *agentv1.StorePutRequest,
) (*agentv1.StorePutResponse, error) {
	s.data.StorePut(req.GetKey(), req.GetValue())
	return &agentv1.StorePutResponse{}, nil
}

func (s *storeServer) Delete(
	ctx context.Context,
	req *agentv1.StoreDeleteRequest,
) (*agentv1.StoreDeleteResponse, error) {
	s.data.StoreDelete(req.GetKey())
	return &agentv1.StoreDeleteResponse{}, nil
}

func (s *storeServer) List(
	ctx context.Context,
	req *agentv1.StoreListRequest,
) (*agentv1.StoreListResponse, error) {
	return &agentv1.StoreListResponse{Keys: s.data.StoreKeys(req.GetPrefix())}, nil
}

// --- Events ---

type eventsServer struct {
	agentv1.UnimplementedEventBusServiceServer
	data     *HostData
	moduleID string
}

func (s *eventsServer) Publish(
	ctx context.Context,
	req *agentv1.PublishRequest,
) (*agentv1.PublishResponse, error) {
	// Core prefixes the topic with the publisher's id; the stub does too.
	s.data.Publish(s.moduleID+"."+req.GetTopic(), req.GetData())
	return &agentv1.PublishResponse{}, nil
}

func (s *eventsServer) Subscribe(
	req *agentv1.SubscribeRequest,
	stream agentv1.EventBusService_SubscribeServer,
) error {
	sub := s.data.subscribe(req.GetTopics())
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev := <-sub.ch:
			if err := stream.Send(&agentv1.Event{
				Topic:         ev.Topic,
				Data:          ev.Data,
				PublishedAtMs: ev.At.UnixMilli(),
			}); err != nil {
				return fmt.Errorf("testkit: sending event: %w", err)
			}
		}
	}
}
