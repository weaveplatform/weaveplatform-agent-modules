package weaveclient

import (
	"context"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// One typed call per capability operation, each a thin wrapper over Call. The
// value is that a caller names an operation rather than spelling a kind string,
// so a typo is a compile error instead of a command the guest silently never
// answers.

// Hello identifies the guest agent, and doubles as the liveness check: a guest
// that is booting, or has no resident agent, simply never replies. Pass a
// context with a deadline and treat expiry as "no agent here".
func (c *Client) Hello(ctx context.Context) (weavewire.HelloResponse, error) {
	var out weavewire.HelloResponse
	err := c.Call(ctx, weavewire.KindPresenceHello, nil, &out)
	return out, err
}

// Inventory asks the guest what it is and what addresses it has.
//
// The addresses are the part a host cannot get any other way: a hypervisor
// knows the MAC it handed the guest, but DHCP, a static configuration or a
// second NIC all happen inside. This backs `weave ip`.
//
// Fields the guest could not read come back empty — data, not failure.
func (c *Client) Inventory(ctx context.Context) (weavewire.InventoryResponse, error) {
	var out weavewire.InventoryResponse
	err := c.Call(ctx, weavewire.KindPresenceInventory, nil, &out)
	return out, err
}

// Shutdown asks the guest OS to power off, reporting whether it accepted.
//
// Accepted is not finished. The command the guest runs terminates the OS that
// is running the agent, so there is no second message: the caller learns it
// completed by the guest powering off. Treat true as "stop hurrying it along".
//
// The path exists because a Windows guest does not act on the ACPI power
// button, so without it the only way to end one is to cut it off mid-write.
func (c *Client) Shutdown(ctx context.Context, reason string) (bool, error) {
	return c.power(ctx, weavewire.KindPowerShutdown, reason)
}

// Restart reboots the guest OS. Same contract as Shutdown.
func (c *Client) Restart(ctx context.Context, reason string) (bool, error) {
	return c.power(ctx, weavewire.KindPowerRestart, reason)
}

func (c *Client) power(ctx context.Context, kind, reason string) (bool, error) {
	var out weavewire.PowerResponse
	if err := c.Call(ctx, kind, weavewire.PowerRequest{Reason: reason}, &out); err != nil {
		return false, err
	}
	return out.Accepted, nil
}

// Time reads the guest's clock without changing it, for measuring drift.
func (c *Client) Time(ctx context.Context) (weavewire.TimeResponse, error) {
	var out weavewire.TimeResponse
	err := c.Call(ctx, weavewire.KindTimeGet, nil, &out)
	return out, err
}

// SetTime corrects the guest's clock, returning how far it moved. See
// weavewire.TimeSetRequest for why the absolute time is sent rather than an
// offset, and what maxSkew guards against.
func (c *Client) SetTime(
	ctx context.Context,
	t time.Time,
	maxSkew time.Duration,
) (weavewire.TimeSetResponse, error) {
	var out weavewire.TimeSetResponse
	err := c.Call(ctx, weavewire.KindTimeSet, weavewire.TimeSetRequest{
		UnixNano:       t.UTC().UnixNano(),
		MaxSkewSeconds: int64(maxSkew / time.Second),
	}, &out)
	return out, err
}

// Metrics takes one live resource sample. CPU is measured over the interval
// between samples, so the FIRST call reports 0 — callers plotting a graph
// should discard that point rather than show it as an idle guest.
func (c *Client) Metrics(ctx context.Context) (weavewire.MetricsResponse, error) {
	var out weavewire.MetricsResponse
	err := c.Call(ctx, weavewire.KindMetricsSample, nil, &out)
	return out, err
}
