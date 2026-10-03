package weavepresence

import (
	"context"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

func (s *Service) handleInventory(ctx context.Context, _ []byte) ([]byte, error) {
	inv := weavewire.InventoryResponse{
		CollectedAt: time.Now().UTC(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		CPUCores:    runtime.NumCPU(),
	}
	inv.Hostname, _ = os.Hostname()
	inv.Interfaces = collectInterfaces()

	if s.inventory != nil {
		s.inventory.Collect(ctx, &inv)
	}
	return weavewire.EncodePayload(inv)
}

// collectInterfaces reads the guest's network interfaces.
//
// This is the portable half, and deliberately so: net.Interfaces gives the same
// answer on all three guest OSes, and it is the answer the host cannot get any
// other way. A hypervisor knows the MAC it handed a guest but not the address
// the guest ended up with — DHCP, a static config, or a second NIC all happen
// inside — so the reason the inventory op exists at all is satisfied without a
// single OS-specific call.
//
// Failures are swallowed: a guest whose interface table cannot be read still
// has a hostname and a serial number worth reporting.
func collectInterfaces() []weavewire.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]weavewire.Interface, 0, len(ifaces))
	for _, iface := range ifaces {
		entry := weavewire.Interface{
			Name:     iface.Name,
			MAC:      iface.HardwareAddr.String(),
			Up:       iface.Flags&net.FlagUp != 0,
			Loopback: iface.Flags&net.FlagLoopback != 0,
			MTU:      iface.MTU,
		}
		// One interface's addresses failing must not cost the whole table —
		// an interface being torn down mid-enumeration is normal.
		if addrs, err := iface.Addrs(); err == nil {
			for _, a := range addrs {
				entry.Addrs = append(entry.Addrs, a.String())
			}
		}
		out = append(out, entry)
	}
	return out
}
