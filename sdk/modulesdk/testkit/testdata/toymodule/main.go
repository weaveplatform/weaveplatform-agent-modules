// Command toymodule is the SDK's integration-test module: it exercises
// each host surface once so the harness can assert the full loop.
package main

import (
	"context"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
)

type toy struct {
	host modulesdk.Host
}

func (t *toy) ID() string { return "toy" }

func (t *toy) Requires() []modulesdk.Capability {
	return []modulesdk.Capability{"test.cap"}
}

func (t *toy) Init(ctx context.Context, host modulesdk.Host) error {
	t.host = host
	// Exercise the store and surface declaration during Init.
	if err := host.Store("").Put(ctx, "init-key", []byte("init-value")); err != nil {
		return err
	}
	return host.UI().Declare(modulesdk.Surface{ID: "toy-card", Title: "Toy", Kind: "card"})
}

func (t *toy) Start(ctx context.Context) error {
	// Exercise the bus and the transport on start.
	if err := t.host.Events().Publish(ctx, "started", []byte("hello")); err != nil {
		return err
	}
	_, err := t.host.Transport().Send(ctx, modulesdk.Message{
		Peer: modulesdk.PeerHypervisor,
		Kind: "heartbeat",
		Data: []byte("beat"),
	}, false)
	return err
}

func (t *toy) Stop(ctx context.Context) error { return nil }

func (t *toy) Health() modulesdk.Health {
	return modulesdk.Health{Status: modulesdk.HealthHealthy}
}

func main() { modulesdk.Serve(&toy{}) }
