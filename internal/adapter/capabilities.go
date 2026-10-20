package adapter

import "sync"

// CapabilityKey includes the observed tool version and environment. A
// capability verified for one version is never inherited by an upgrade.
type CapabilityKey struct {
	Tool        string
	Version     string
	Environment Environment
}

type Registry struct {
	mu      sync.RWMutex
	entries map[CapabilityKey]Capabilities
}

func NewRegistry() *Registry { return &Registry{entries: make(map[CapabilityKey]Capabilities)} }
func (r *Registry) Register(key CapabilityKey, capabilities Capabilities) {
	r.mu.Lock()
	r.entries[key] = capabilities
	r.mu.Unlock()
}
func (r *Registry) Lookup(key CapabilityKey) (Capabilities, bool) {
	r.mu.RLock()
	capabilities, ok := r.entries[key]
	r.mu.RUnlock()
	return capabilities, ok
}
