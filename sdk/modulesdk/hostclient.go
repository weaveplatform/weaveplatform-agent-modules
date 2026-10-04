package modulesdk

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/ipc"
)

// hostClient implements Host over core's per-module host socket. All auth
// is per-connection: the one-time token rides every RPC as metadata and
// core binds the connection to this module's namespace on first use.
type hostClient struct {
	conn *grpc.ClientConn
	log  *slog.Logger

	identity  agentv1.IdentityServiceClient
	transport agentv1.TransportServiceClient
	policy    agentv1.PolicyServiceClient
	store     agentv1.StoreServiceClient
	events    agentv1.EventBusServiceClient
	logs      agentv1.LogServiceClient
	watchdog  agentv1.WatchdogServiceClient
	registry  agentv1.RegistryServiceClient

	streamedLog *slog.Logger

	mu       sync.Mutex
	surfaces []Surface
	jobs     []Job
	// watchdogInterval is the cadence core asked for in InitRequest; zero
	// disables the push watchdog.
	watchdogInterval time.Duration
	// jobCancel is non-nil while jobs are running (between Start and Stop).
	jobCancel context.CancelFunc
	jobWG     sync.WaitGroup
	started   bool
}

func dialHost(network, addr, token string, log *slog.Logger) (*hostClient, error) {
	withToken := func(ctx context.Context) context.Context {
		return metadata.AppendToOutgoingContext(ctx, handshake.TokenMetadataKey, token)
	}
	conn, err := ipc.GRPCClient(network, addr,
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any,
			cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
		) error {
			return sentinelErr(invoker(withToken(ctx), method, req, reply, cc, opts...))
		}),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc,
			cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption,
		) (grpc.ClientStream, error) {
			return streamer(withToken(ctx), desc, cc, method, opts...)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("modulesdk: dialing host: %w", err)
	}
	hc := &hostClient{
		conn:      conn,
		log:       log,
		identity:  agentv1.NewIdentityServiceClient(conn),
		transport: agentv1.NewTransportServiceClient(conn),
		policy:    agentv1.NewPolicyServiceClient(conn),
		store:     agentv1.NewStoreServiceClient(conn),
		events:    agentv1.NewEventBusServiceClient(conn),
		logs:      agentv1.NewLogServiceClient(conn),
		watchdog:  agentv1.NewWatchdogServiceClient(conn),
		registry:  agentv1.NewRegistryServiceClient(conn),
	}
	// Host.Log streams to core's LogService with the stderr logger as the
	// pre-Init / on-failure fallback.
	hc.streamedLog = slog.New(newStreamHandler(log.Handler(), hc.logs))
	return hc, nil
}

func (h *hostClient) Close() error {
	if err := h.conn.Close(); err != nil {
		return fmt.Errorf("modulesdk: closing host connection: %w", err)
	}
	return nil
}

// awaitDisconnect blocks until the connection to core's host services is
// permanently gone, then calls onLost. It nudges the connection back to
// Ready when it can, so only a genuine loss (core exited) — not a
// transient blip — triggers the callback. This is the module's
// orphan-death mechanism on platforms without Pdeathsig.
//
// If ctx ends first the watch stops without calling onLost.
func (h *hostClient) awaitDisconnect(ctx context.Context, onLost func()) {
	for {
		state := h.conn.GetState()
		if state == connectivity.Shutdown {
			onLost()
			return
		}
		if state == connectivity.TransientFailure {
			// The local socket peer is gone; for a unix-socket/pipe to
			// core this does not recover — core is dead.
			onLost()
			return
		}
		h.conn.Connect()
		if !h.conn.WaitForStateChange(ctx, state) {
			return
		}
	}
}

func (h *hostClient) Log() *slog.Logger {
	if h.streamedLog != nil {
		return h.streamedLog
	}
	return h.log
}

// --- Identity ---

func (h *hostClient) Identity() Identity { return identityClient{h} }

type identityClient struct{ h *hostClient }

func (c identityClient) WhoAmI(ctx context.Context) (DeviceIdentity, error) {
	resp, err := c.h.identity.WhoAmI(ctx, &agentv1.WhoAmIRequest{})
	if err != nil {
		return DeviceIdentity{}, err
	}
	return DeviceIdentity{
		DeviceID:  resp.GetDeviceId(),
		Ephemeral: resp.GetEphemeral(),
		Tenant:    resp.GetTenant(),
	}, nil
}

func (c identityClient) Credential(ctx context.Context, scopes []string) (Credential, error) {
	resp, err := c.h.identity.Credential(ctx, &agentv1.CredentialRequest{Scopes: scopes})
	if err != nil {
		return Credential{}, err
	}
	return Credential{
		Token:     resp.GetToken(),
		ExpiresAt: time.Unix(resp.GetExpiresAt(), 0),
		Scopes:    resp.GetGrantedScopes(),
	}, nil
}

// --- Transport ---

func (h *hostClient) Transport() Transport { return transportClient{h} }

type transportClient struct{ h *hostClient }

func (c transportClient) Send(ctx context.Context, msg Message, queueOffline bool) (bool, error) {
	resp, err := c.h.transport.Send(ctx, &agentv1.TransportSendRequest{
		Message: &agentv1.TransportMessage{
			Peer: msg.Peer.wire(),
			Kind: msg.Kind,
			Data: msg.Data,
		},
		QueueOffline: queueOffline,
	})
	if err != nil {
		return false, err
	}
	return resp.GetDelivered(), nil
}

func (c transportClient) Receive(ctx context.Context) (<-chan Message, error) {
	stream, err := c.h.transport.Receive(ctx, &agentv1.TransportReceiveRequest{})
	if err != nil {
		return nil, err
	}
	out := make(chan Message)
	go func() {
		defer close(out)
		for {
			m, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case out <- Message{Peer: Peer(m.GetPeer()), Kind: m.GetKind(), Data: m.GetData()}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// --- Policy ---

func (h *hostClient) Policy() PolicyReader { return policyClient{h} }

// policyDoc maps the wire document to the SDK type, including the envelope
// metadata (schema version, content type).
func policyDoc(doc *agentv1.PolicyDocument) PolicyDocument {
	return PolicyDocument{
		Revision:      doc.GetRevision(),
		Data:          doc.GetData(),
		SchemaVersion: doc.GetSchemaVersion(),
		ContentType:   doc.GetContentType(),
	}
}

type policyClient struct{ h *hostClient }

func (c policyClient) Get(ctx context.Context) (PolicyDocument, error) {
	doc, err := c.h.policy.Get(ctx, &agentv1.PolicyGetRequest{})
	if err != nil {
		return PolicyDocument{}, err
	}
	return policyDoc(doc), nil
}

func (c policyClient) Watch(ctx context.Context) (<-chan PolicyDocument, error) {
	stream, err := c.h.policy.Watch(ctx, &agentv1.PolicyWatchRequest{})
	if err != nil {
		return nil, err
	}
	out := make(chan PolicyDocument)
	go func() {
		defer close(out)
		for {
			doc, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case out <- policyDoc(doc):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// --- Store ---

func (h *hostClient) Store(ns string) Store {
	prefix := ""
	if ns != "" {
		prefix = ns + "/"
	}
	return storeClient{h: h, prefix: prefix}
}

type storeClient struct {
	h      *hostClient
	prefix string
}

func (c storeClient) Get(ctx context.Context, key string) ([]byte, bool, error) {
	resp, err := c.h.store.Get(ctx, &agentv1.StoreGetRequest{Key: c.prefix + key})
	if err != nil {
		return nil, false, err
	}
	return resp.GetValue(), resp.GetFound(), nil
}

func (c storeClient) Put(ctx context.Context, key string, value []byte) error {
	_, err := c.h.store.Put(ctx, &agentv1.StorePutRequest{Key: c.prefix + key, Value: value})
	return err
}

func (c storeClient) Delete(ctx context.Context, key string) error {
	_, err := c.h.store.Delete(ctx, &agentv1.StoreDeleteRequest{Key: c.prefix + key})
	return err
}

func (c storeClient) List(ctx context.Context, prefix string) ([]string, error) {
	resp, err := c.h.store.List(ctx, &agentv1.StoreListRequest{Prefix: c.prefix + prefix})
	if err != nil {
		return nil, err
	}
	keys := resp.GetKeys()
	if c.prefix != "" {
		trimmed := make([]string, 0, len(keys))
		for _, k := range keys {
			trimmed = append(trimmed, strings.TrimPrefix(k, c.prefix))
		}
		keys = trimmed
	}
	return keys, nil
}

// --- Events ---

func (h *hostClient) Events() Events { return eventsClient{h} }

type eventsClient struct{ h *hostClient }

func (c eventsClient) Publish(ctx context.Context, topic string, data []byte) error {
	_, err := c.h.events.Publish(ctx, &agentv1.PublishRequest{Topic: topic, Data: data})
	return err
}

func (c eventsClient) Subscribe(ctx context.Context, topics ...string) (<-chan Event, error) {
	stream, err := c.h.events.Subscribe(ctx, &agentv1.SubscribeRequest{Topics: topics})
	if err != nil {
		return nil, err
	}
	out := make(chan Event)
	go func() {
		defer close(out)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case out <- Event{
				Topic:       ev.GetTopic(),
				Data:        ev.GetData(),
				PublishedAt: time.UnixMilli(ev.GetPublishedAtMs()),
				Sequence:    ev.GetSequence(),
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// --- UI ---

func (h *hostClient) UI() UIBroker { return uiBroker{h} }

type uiBroker struct{ h *hostClient }

func (b uiBroker) Declare(surfaces ...Surface) error {
	b.h.mu.Lock()
	defer b.h.mu.Unlock()
	if b.h.started {
		return ErrSurfacesAfterInit
	}
	b.h.surfaces = append(b.h.surfaces, surfaces...)
	return nil
}

// declaredSurfaces returns surfaces recorded during Init, for the Init
// response.
func (h *hostClient) declaredSurfaces() []*agentv1.Surface {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*agentv1.Surface, 0, len(h.surfaces))
	for _, s := range h.surfaces {
		out = append(out, &agentv1.Surface{Id: s.ID, Title: s.Title, Kind: s.Kind, Data: s.Data})
	}
	return out
}

// --- Schedule ---

// addJobs records the module's declared jobs (from Scheduled.Jobs) before
// Start. The runtime, not the module, owns their lifecycle.
func (h *hostClient) addJobs(jobs []Job) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.jobs = append(h.jobs, jobs...)
}

// setWatchdogInterval records the cadence core asked for in InitRequest.
// Called by the runtime during Init, before startJobs.
func (h *hostClient) setWatchdogInterval(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.watchdogInterval = d
}

// startJobs begins all registered jobs. Called by the runtime on Start.
func (h *hostClient) startJobs(parent context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.jobCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	h.jobCancel = cancel
	h.started = true
	for _, j := range h.jobs {
		h.launchJobLocked(ctx, j)
	}
	if h.watchdogInterval > 0 {
		h.launchWatchdogLocked(ctx, h.watchdogInterval)
	}
}

// launchWatchdogLocked runs the push-watchdog ping loop for the runtime's
// lifetime (cancelled on Stop). It opens WatchdogService.Notify and sends a
// ping every interval; a broken stream is reopened on the next tick so a
// transient blip does not silently end liveness reporting.
func (h *hostClient) launchWatchdogLocked(ctx context.Context, interval time.Duration) {
	h.jobWG.Add(1)
	go func() {
		defer h.jobWG.Done()
		var (
			stream agentv1.WatchdogService_NotifyClient
			seq    uint64
		)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				if stream != nil {
					_, _ = stream.CloseAndRecv()
				}
				return
			case <-t.C:
				if stream == nil {
					s, err := h.watchdog.Notify(ctx)
					if err != nil {
						continue // retry on the next tick
					}
					stream = s
				}
				seq++
				if err := stream.Send(&agentv1.WatchdogPing{Sequence: seq}); err != nil {
					stream = nil // reopen next tick
				}
			}
		}
	}()
}

func (h *hostClient) launchJobLocked(ctx context.Context, j Job) {
	h.jobWG.Add(1)
	go func() {
		defer h.jobWG.Done()
		j.Run(ctx)
		// Re-read the interval each cycle so a policy change takes effect,
		// rather than capturing it once at schedule time. EveryFunc wins
		// over the static Every when set.
		next := func() time.Duration {
			if j.EveryFunc != nil {
				return j.EveryFunc()
			}
			return j.Every
		}
		for {
			d := next()
			if d <= 0 {
				return
			}
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				j.Run(ctx)
			}
		}
	}()
}

// stopJobs cancels all jobs and waits for them. Called by the runtime on
// Stop and Shutdown.
func (h *hostClient) stopJobs() {
	h.mu.Lock()
	cancel := h.jobCancel
	h.jobCancel = nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
		h.jobWG.Wait()
	}
}
