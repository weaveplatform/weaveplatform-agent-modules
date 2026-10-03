package weaveclient

import (
	"context"
	"encoding/json"
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

// SessionCurrent reports the session at the guest's physical console, or nil
// when nobody is logged in there. Session runs as system, so this answers
// either way — ask it when a clipboard or display call fails with
// ErrNoSession, to tell "nobody is logged in" from "not installed".
func (c *Client) SessionCurrent(ctx context.Context) (*weavewire.SessionInfo, error) {
	var out weavewire.SessionCurrentResponse
	if err := c.Call(ctx, weavewire.KindSessionCurrent, nil, &out); err != nil {
		return nil, err
	}
	return out.Session, nil
}

// SessionList reports every logged-in session the guest can enumerate. An OS
// that cannot answers ErrUnsupported.
func (c *Client) SessionList(ctx context.Context) ([]weavewire.SessionInfo, error) {
	var out weavewire.SessionListResponse
	err := c.Call(ctx, weavewire.KindSessionList, nil, &out)
	return out.Sessions, err
}

// SessionLock locks a session's screen — the console session when sessionID
// is empty — and returns the id of the session it locked.
func (c *Client) SessionLock(ctx context.Context, sessionID string) (string, error) {
	var out weavewire.SessionLockResponse
	err := c.Call(
		ctx,
		weavewire.KindSessionLock,
		weavewire.SessionLockRequest{SessionID: sessionID},
		&out,
	)
	return out.SessionID, err
}

// OnSessionChanged calls fn for every change of the guest's console session:
// a login, a logout, a lock or unlock, a fast user switch. Like On, it runs on
// the read loop and must not block, and it stays registered for the life of
// the client.
//
// A logout or user switch also restarts the per-user-console modules, so a
// clipboard sync resets its change state here.
func (c *Client) OnSessionChanged(fn func(weavewire.SessionChangedEvent)) {
	c.On(weavewire.KindSessionChanged, func(_ string, data []byte) {
		var ev weavewire.SessionChangedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			c.log.Warn("weaveclient: undecodable session change", "err", err)
			return
		}
		fn(ev)
	})
}

// DisplayList reports the displays of the guest's console session. With
// nobody logged in it fails with ErrNoSession after Options.SessionTimeout.
func (c *Client) DisplayList(ctx context.Context) ([]weavewire.DisplayInfo, error) {
	var out weavewire.DisplayListResponse
	err := c.Call(ctx, weavewire.KindDisplayList, nil, &out)
	return out.Displays, err
}

// DisplaySet changes a display's resolution or scale and returns the display
// as it is afterwards — trust that over the request, since an OS may round a
// custom mode. An OS that cannot change displays answers ErrUnsupported.
func (c *Client) DisplaySet(
	ctx context.Context,
	req weavewire.DisplaySetRequest,
) (weavewire.DisplayInfo, error) {
	var out weavewire.DisplaySetResponse
	err := c.Call(ctx, weavewire.KindDisplaySet, req, &out)
	return out.Display, err
}
