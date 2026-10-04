package testkit

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/ipc"
)

// StubCore performs the core side of the handshake against a real module
// binary. It is the integration harness for the SDK runtime and the
// protocol-compat fixture: point it at a module binary, choose the window
// core advertises, and drive the lifecycle.
type StubCore struct {
	// Window is what core advertises. Zero value means {1, 1}.
	Window handshake.Window
	// ModuleID is what core believes it is launching.
	ModuleID string
	// Config is delivered in InitRequest.
	Config []byte
	// Capabilities delivered in InitRequest.
	Capabilities []string
	// Data backs the host services. Nil gets a fresh NewHostData.
	Data *HostData
	// LaunchTimeout bounds waiting for the handshake line. Zero means 10s.
	LaunchTimeout time.Duration
}

// ErrProtocolRefused reports that the module exited with
// handshake.ExitProtocolUnsupported instead of answering — the clean
// refusal the protocol demands.
var ErrProtocolRefused = errors.New("module refused protocol window")

// ErrHandshakeTimeout reports a module that neither answered the handshake
// nor exited within LaunchTimeout.
var ErrHandshakeTimeout = errors.New("timeout waiting for handshake line")

// ErrProtocolOutOfWindow reports a module that answered the handshake with a
// protocol outside the window the stub advertised: the refusal it should
// have made with an exit code, made wrongly.
var ErrProtocolOutOfWindow = errors.New("module claimed a protocol outside the advertised window")

// ModuleProc is a launched module under the stub's control.
type ModuleProc struct {
	// Client speaks ModuleService to the module.
	Client agentv1.ModuleServiceClient
	// Line is the parsed handshake answer.
	Line handshake.Line
	// Data is the host-side state the module acts on.
	Data *HostData

	cmd        *exec.Cmd
	conn       *grpc.ClientConn
	hostServer *grpc.Server
	stderr     *bufio.Scanner
}

// Launch spawns the module binary, serves host services, and completes the
// handshake. On a clean protocol refusal it returns ErrProtocolRefused.
func (c *StubCore) Launch(ctx context.Context, binPath string) (*ModuleProc, error) {
	window := c.Window
	if window == (handshake.Window{}) {
		window = handshake.Window{Min: 1, Max: 1}
	}
	data := c.Data
	if data == nil {
		data = NewHostData()
	}
	timeout := c.LaunchTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("testkit: handshake token: %w", err)
	}
	token := hex.EncodeToString(nonce[:])

	// Host services listener.
	sockDir, err := os.MkdirTemp("", "weave-stubcore-*")
	if err != nil {
		return nil, fmt.Errorf("testkit: socket directory: %w", err)
	}
	hostAddr := filepath.Join(sockDir, "host.sock")
	if runtime.GOOS == "windows" {
		hostAddr = `\\.\pipe\weave-stubhost-` + token
	}
	hostLis, err := ipc.Listen(hostAddr)
	if err != nil {
		return nil, err
	}
	// Core lists every module it supervises, the one asking included.
	if c.ModuleID != "" && !data.hasRegistryModule(c.ModuleID) {
		data.SetRegistryModule(RegistryEntry{
			ID:     c.ModuleID,
			State:  "running",
			Health: agentv1.Health_STATUS_UNSPECIFIED,
		})
	}
	hostSrv := grpc.NewServer(tokenInterceptors(token)...)
	registerHostServices(hostSrv, data, c.ModuleID)
	go hostSrv.Serve(hostLis) //nolint:errcheck // exits with GracefulStop

	// Spawn. The module outlives ctx, which bounds the launch only: the
	// ModuleProc owns the process from here and stops it.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), binPath)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=%d", handshake.EnvProtocolMin, window.Min),
		fmt.Sprintf("%s=%d", handshake.EnvProtocolMax, window.Max),
		handshake.EnvToken+"="+token,
		handshake.EnvHostAddr+"="+hostAddr,
		handshake.EnvSocketDir+"="+sockDir,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		hostSrv.Stop()
		return nil, fmt.Errorf("testkit: module stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		hostSrv.Stop()
		return nil, fmt.Errorf("testkit: module stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		hostSrv.Stop()
		return nil, fmt.Errorf("starting %s: %w", binPath, err)
	}

	// Read the one handshake line, bounded.
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			lineCh <- sc.Text()
			// Drain any further stdout so the pipe never blocks.
			io.Copy(io.Discard, stdout) //nolint:errcheck
			return
		}
		errCh <- fmt.Errorf("module exited before handshake: %w", cmd.Wait())
	}()

	var rawLine string
	select {
	case rawLine = <-lineCh:
	case err := <-errCh:
		hostSrv.Stop()
		if state := cmd.ProcessState; state != nil &&
			state.ExitCode() == handshake.ExitProtocolUnsupported {
			return nil, ErrProtocolRefused
		}
		return nil, err
	case <-time.After(timeout):
		cmd.Process.Kill() //nolint:errcheck
		hostSrv.Stop()
		return nil, fmt.Errorf("%w after %s", ErrHandshakeTimeout, timeout)
	case <-ctx.Done():
		cmd.Process.Kill() //nolint:errcheck
		hostSrv.Stop()
		return nil, ctx.Err()
	}

	line, err := handshake.Parse(rawLine)
	if err != nil {
		cmd.Process.Kill() //nolint:errcheck
		hostSrv.Stop()
		return nil, err
	}
	if !window.Contains(line.Protocol) {
		cmd.Process.Kill() //nolint:errcheck
		hostSrv.Stop()
		return nil, fmt.Errorf("%w: protocol %d, window [%d,%d]",
			ErrProtocolOutOfWindow, line.Protocol, window.Min, window.Max)
	}

	conn, err := ipc.GRPCClient(line.Network, line.Addr)
	if err != nil {
		cmd.Process.Kill() //nolint:errcheck
		hostSrv.Stop()
		return nil, err
	}

	return &ModuleProc{
		Client:     agentv1.NewModuleServiceClient(conn),
		Line:       line,
		Data:       data,
		cmd:        cmd,
		conn:       conn,
		hostServer: hostSrv,
		stderr:     bufio.NewScanner(stderr),
	}, nil
}

// Init drives ModuleService.Init with the stub's identity and config.
func (c *StubCore) Init(ctx context.Context, p *ModuleProc) (*agentv1.InitResponse, error) {
	caps := make([]*agentv1.Capability, 0, len(c.Capabilities))
	for _, name := range c.Capabilities {
		caps = append(caps, &agentv1.Capability{Name: name})
	}
	return p.Client.Init(ctx, &agentv1.InitRequest{
		Protocol:     p.Line.Protocol,
		ModuleId:     c.ModuleID,
		Capabilities: caps,
		Config:       c.Config,
		Privilege:    agentv1.PrivilegeLevel_PRIVILEGE_LEVEL_SERVICE,
	})
}

// Shutdown asks the module to exit and waits for the process, bounded by
// ctx. It returns the process exit error, if any.
func (p *ModuleProc) Shutdown(ctx context.Context) error {
	_, err := p.Client.Shutdown(ctx, &agentv1.ShutdownRequest{})
	if err != nil {
		return err
	}
	return p.WaitExit(ctx)
}

// WaitExit waits for the module process to end.
func (p *ModuleProc) WaitExit(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	defer func() {
		p.conn.Close() //nolint:errcheck
		p.hostServer.GracefulStop()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		p.cmd.Process.Kill() //nolint:errcheck
		<-done
		return ctx.Err()
	}
}

// Kill hard-stops the module process and tears the harness down.
func (p *ModuleProc) Kill() {
	p.cmd.Process.Kill() //nolint:errcheck
	p.cmd.Wait()         //nolint:errcheck
	p.conn.Close()       //nolint:errcheck
	p.hostServer.Stop()
}
