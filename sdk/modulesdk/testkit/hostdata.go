// Package testkit is the test harness for modules and for the module
// runtime itself. It provides an in-memory implementation of core's host
// services and a StubCore that spawns a real module binary, performs the
// core side of the handshake, and drives its lifecycle — the same harness
// the protocol-compat CI fixture uses.
package testkit

import (
	"slices"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
)

// HostData is the in-memory state behind the stub host services. Tests
// read and mutate it directly.
type HostData struct {
	mu sync.Mutex

	// Store holds what modules Put, keyed as sent (namespace prefixes
	// included).
	store map[string][]byte

	// Policy is the document served to Get and Watch.
	policyRevision uint64
	policyData     []byte
	policyWatchers []chan struct{}

	// Events records everything published; subscribers receive live.
	events      []PublishedEvent
	subscribers []*subscriber

	// Sends records everything the module sent via Transport.
	sends []SentMessage

	// registry is what RegistryService serves, by module id.
	registryRevision    uint64
	registry            map[string]RegistryEntry
	registryWatchers    []chan struct{}
	registryUnsupported bool

	// Device is what WhoAmI returns.
	Device DeviceInfo
}

// PublishedEvent is one recorded bus publish, topic already prefixed with
// the module id as core would.
type PublishedEvent struct {
	Topic string
	Data  []byte
	At    time.Time
}

// SentMessage is one recorded Transport.Send.
type SentMessage struct {
	Peer         int32
	Kind         string
	Data         []byte
	QueueOffline bool
}

// DeviceInfo is the stub identity.
type DeviceInfo struct {
	DeviceID  string
	Ephemeral bool
	Tenant    string
}

type subscriber struct {
	topics []string
	ch     chan PublishedEvent
}

// NewHostData returns empty backing state with a default device identity.
func NewHostData() *HostData {
	return &HostData{
		store:    make(map[string][]byte),
		registry: make(map[string]RegistryEntry),
		Device:   DeviceInfo{DeviceID: "test-device", Tenant: "test"},
	}
}

// StoreGet returns a stored value.
func (d *HostData) StoreGet(key string) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.store[key]
	return v, ok
}

// StorePut writes a value (as core would on the module's behalf).
func (d *HostData) StorePut(key string, value []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.store[key] = value
}

// StoreDelete removes a key.
func (d *HostData) StoreDelete(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.store, key)
}

// StoreKeys lists keys with the prefix.
func (d *HostData) StoreKeys(prefix string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys []string
	for k := range d.store {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			keys = append(keys, k)
		}
	}
	return keys
}

// SetPolicy replaces the policy document and wakes watchers.
func (d *HostData) SetPolicy(data []byte) {
	d.mu.Lock()
	d.policyRevision++
	d.policyData = data
	watchers := d.policyWatchers
	d.mu.Unlock()
	for _, w := range watchers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// Policy returns the current document.
func (d *HostData) Policy() (uint64, []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.policyRevision, d.policyData
}

func (d *HostData) addPolicyWatcher() chan struct{} {
	ch := make(chan struct{}, 1)
	d.mu.Lock()
	d.policyWatchers = append(d.policyWatchers, ch)
	d.mu.Unlock()
	return ch
}

// Publish records an event and fans it out to matching subscribers.
func (d *HostData) Publish(topic string, data []byte) {
	ev := PublishedEvent{Topic: topic, Data: data, At: time.Now()}
	d.mu.Lock()
	d.events = append(d.events, ev)
	subs := d.subscribers
	d.mu.Unlock()
	for _, s := range subs {
		if topicMatches(s.topics, topic) {
			select {
			case s.ch <- ev:
			default:
			}
		}
	}
}

// Events returns all recorded publishes.
func (d *HostData) Events() []PublishedEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]PublishedEvent(nil), d.events...)
}

func (d *HostData) subscribe(topics []string) *subscriber {
	s := &subscriber{topics: topics, ch: make(chan PublishedEvent, 64)}
	d.mu.Lock()
	d.subscribers = append(d.subscribers, s)
	d.mu.Unlock()
	return s
}

// RecordSend appends a transport send.
func (d *HostData) RecordSend(m SentMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sends = append(d.sends, m)
}

// Sends returns all recorded transport sends.
func (d *HostData) Sends() []SentMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]SentMessage(nil), d.sends...)
}

// RegistryEntry is one module in the stub's RegistryService.
type RegistryEntry struct {
	ID      string
	Version string
	// Address defaults to ID.
	Address string
	// State defaults to "running".
	State        string
	Health       agentv1.Health_Status
	HealthReason string
}

// SetRegistryModule adds e to the registry, or replaces the entry with its
// ID, moving the revision and waking watchers. StubCore.Launch adds the
// launched module itself, as core lists every module it supervises.
func (d *HostData) SetRegistryModule(e RegistryEntry) {
	if e.Address == "" {
		e.Address = e.ID
	}
	if e.State == "" {
		e.State = "running"
	}
	d.changeRegistry(func() { d.registry[e.ID] = e })
}

// RemoveRegistryModule drops the entry with id.
func (d *HostData) RemoveRegistryModule(id string) {
	d.changeRegistry(func() { delete(d.registry, id) })
}

// SetRegistryUnsupported makes RegistryService answer Unimplemented, as a
// core from before weave-agent v0.9.2 does.
func (d *HostData) SetRegistryUnsupported(unsupported bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.registryUnsupported = unsupported
}

// Registry returns the revision and the entries, sorted by id.
func (d *HostData) Registry() (uint64, []RegistryEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]RegistryEntry, 0, len(d.registry))
	for _, e := range d.registry {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b RegistryEntry) int { return strings.Compare(a.ID, b.ID) })
	return d.registryRevision, out
}

func (d *HostData) hasRegistryModule(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.registry[id]
	return ok
}

func (d *HostData) changeRegistry(f func()) {
	d.mu.Lock()
	f()
	d.registryRevision++
	watchers := d.registryWatchers
	d.mu.Unlock()
	for _, w := range watchers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

func (d *HostData) addRegistryWatcher() chan struct{} {
	ch := make(chan struct{}, 1)
	d.mu.Lock()
	d.registryWatchers = append(d.registryWatchers, ch)
	d.mu.Unlock()
	return ch
}

func (d *HostData) registrySupported() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.registryUnsupported
}

// topicMatches implements exact and "prefix.*" glob matching.
func topicMatches(patterns []string, topic string) bool {
	for _, p := range patterns {
		if p == topic {
			return true
		}
		if n := len(p); n > 1 && p[n-1] == '*' && len(topic) >= n-1 && topic[:n-1] == p[:n-1] {
			return true
		}
	}
	return false
}
