package modulesdk

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/ipc"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/wlog"
)

// Serve runs m as a Weave platform module: it reads the handshake
// environment, negotiates the protocol, listens on the module socket,
// answers with the one-line handshake, and serves core's lifecycle RPCs
// until Shutdown or SIGTERM.
//
// A module's main is one line:
//
//	func main() { modulesdk.Serve(example.New()) }
//
// Serve does not return. A protocol outside core's advertised window exits
// with code 78 (EX_CONFIG) before listening; other failures exit 1.
// errProtocolUnsupported is returned by serve when the module's protocol
// falls outside core's advertised window. Serve maps it to exit code 78
// so that every process-exit decision lives in one place.
var errProtocolUnsupported = errors.New("protocol out of window")

// errNotLaunchedByCore reports a handshake environment core did not set: the
// binary was run by hand, or by something that is not core.
var errNotLaunchedByCore = errors.New("modules are launched by core, not by hand")

// Process-level effects, as variables so the runtime can be driven in-process
// by tests: there, stdout is the test's own output, a real SIGTERM would kill
// the test binary, and os.Exit would end it.
var (
	handshakeOut  io.Writer = os.Stdout
	notifySignals           = signal.Notify
	exit                    = os.Exit
)

func Serve(m Module) {
	log := wlog.Default(m.ID())
	err := serve(m, log)
	switch {
	case err == nil:
		exit(0)
	case errors.Is(err, errProtocolUnsupported):
		fmt.Fprintln(os.Stderr, err.Error())
		exit(handshake.ExitProtocolUnsupported)
	default:
		log.Error("module runtime failed", "err", err)
		exit(1)
	}
}

func serve(m Module, log *slog.Logger) error {
	// 1. Protocol negotiation: refuse cleanly before touching anything.
	window, err := handshake.ParseWindow(
		os.Getenv(handshake.EnvProtocolMin),
		os.Getenv(handshake.EnvProtocolMax),
	)
	if err != nil {
		return fmt.Errorf("reading protocol window: %w", err)
	}
	if !window.Contains(Protocol) {
		return fmt.Errorf("module %s speaks protocol %d, outside advertised window [%d,%d]: %w",
			m.ID(), Protocol, window.Min, window.Max, errProtocolUnsupported)
	}

	token := os.Getenv(handshake.EnvToken)
	hostAddr := os.Getenv(handshake.EnvHostAddr)
	if token == "" || hostAddr == "" {
		return fmt.Errorf(
			"handshake environment incomplete: %s/%s unset: %w",
			handshake.EnvToken,
			handshake.EnvHostAddr,
			errNotLaunchedByCore,
		)
	}

	// 2. Listen on the module socket.
	addr, err := moduleAddr(m.ID())
	if err != nil {
		return err
	}
	lis, err := ipc.Listen(addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer lis.Close()

	// 3. Wire the lifecycle server. The host connection is deferred to
	// Init: core's host listener is guaranteed up by then.
	srv := &moduleServer{
		m:        m,
		shutdown: make(chan struct{}, 1),
		hostLost: make(chan struct{}, 1),
		connectHost: func() (*hostClient, error) {
			return dialHost(ipc.Network(), hostAddr, token, log)
		},
	}
	grpcServer := grpc.NewServer()
	agentv1.RegisterModuleServiceServer(grpcServer, srv)
	// Serve the standard gRPC health protocol alongside the rich
	// ModuleService.Health: generic tooling (grpc_health_probe, meshes)
	// can liveness-check a module, while core keeps using ModuleService for
	// the DEGRADED/UNHEALTHY nuance stock health lacks. The SDK reports
	// SERVING once the module is up.
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthSrv.SetServingStatus(m.ID(), healthpb.HealthCheckResponse_SERVING)

	errCh := make(chan error, 1)
	go func() { errCh <- grpcServer.Serve(lis) }()

	// 4. Answer the handshake. Exactly one line, then never print to
	// stdout again.
	fmt.Fprintln(
		handshakeOut,
		handshake.Line{Protocol: Protocol, Network: ipc.Network(), Addr: addr}.Format(),
	)

	// 5. Run until Shutdown, a lost core connection, SIGTERM, or server
	// failure.
	sig := make(chan os.Signal, 1)
	notifySignals(sig, syscall.SIGTERM, os.Interrupt)
	select {
	case <-srv.shutdown:
		log.Info("shutdown requested by core")
	case <-srv.hostLost:
		log.Warn("core connection lost; exiting to avoid orphaning")
	case s := <-sig:
		log.Info("signal received", "signal", s.String())
	case err := <-errCh:
		return fmt.Errorf("grpc server: %w", err)
	}
	grpcServer.GracefulStop()
	if host := srv.getHost(); host != nil {
		host.stopJobs()
		host.Close()
	}
	return nil
}

// moduleAddr picks the module's listen address: a socket in core's
// directory on unix, a uniquely-suffixed pipe name on Windows.
func moduleAddr(id string) (string, error) {
	if runtime.GOOS == "windows" {
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", fmt.Errorf("modulesdk: pipe name nonce: %w", err)
		}
		return `\\.\pipe\weave-` + id + "-" + hex.EncodeToString(nonce[:]), nil
	}
	dir := os.Getenv(handshake.EnvSocketDir)
	if dir == "" {
		return "", fmt.Errorf("%s unset: %w", handshake.EnvSocketDir, errNotLaunchedByCore)
	}
	return filepath.Join(dir, id+".sock"), nil
}
