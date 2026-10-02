// Package weavepresence is the presence capability: hello, which answers
// before the channel authenticates so a host can tell "wrong key" from "no
// agent", and inventory, the guest's own account of what it is and which
// addresses it holds.
package weavepresence

import (
	"context"
	"os"
	"runtime"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// KernelBackend supplies the OS-specific kernel string for hello; the rest of
// the hello response (version, os, arch, hostname) is OS-agnostic.
type KernelBackend interface {
	Kernel(ctx context.Context) (string, error)
}

// InventoryBackend fills in the facts only the OS can answer. The portable
// half — hostname, OS, arch, and the network interfaces — is collected here, so
// a backend implements the sysctl/WMI/procfs part and nothing else.
//
// It is best-effort by contract: leave a field zero rather than returning an
// error for it. An inventory missing a serial number is still worth having.
type InventoryBackend interface {
	Collect(ctx context.Context, inv *weavewire.InventoryResponse)
}

// Service is the presence capability. Either backend may be nil: hello and
// inventory both still answer with their portable halves, because a presence
// module that refused to say hello would look like no agent at all.
type Service struct {
	kernel    KernelBackend
	inventory InventoryBackend
}

// NewService builds the presence service.
func NewService(kernel KernelBackend, inventory InventoryBackend) *Service {
	return &Service{kernel: kernel, inventory: inventory}
}

// Capability implements weavemodule.Service.
func (s *Service) Capability() weavewire.Capability { return weavewire.Presence }

// Register implements weavemodule.Service.
func (s *Service) Register(r *weavemodule.Registrar) error {
	r.Handle(weavewire.KindPresenceHello, s.handleHello)
	r.Handle(weavewire.KindPresenceInventory, s.handleInventory)
	return nil
}

func (s *Service) handleHello(ctx context.Context, _ []byte) ([]byte, error) {
	var kernel string
	if s.kernel != nil {
		kernel, _ = s.kernel.Kernel(ctx)
	}
	host, _ := os.Hostname()
	return weavewire.EncodePayload(weavewire.HelloResponse{
		Version:  weavewire.ProtocolVersion,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Kernel:   kernel,
		Hostname: host,
	})
}
