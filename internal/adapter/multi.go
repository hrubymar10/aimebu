package adapter

import (
	"context"
	"fmt"
	"sync"
)

// Multi routes each delivery to the adapter advertised for its tool.
type Multi struct {
	adapters map[string]Adapter
	// catalogs holds what each adapter reported when the Multi was made. An adapter's Catalog runs its CLI to read the version, and
	// the supervisor asks for the catalog of every adapter once per advertised entry; asking each CLI again every time cost seconds
	// at start. The supervisor reads the catalog at start, so a snapshot is what it always meant.
	catalogs map[Adapter][]CatalogEntry
}

func adapterKey(tool, variant string) string { return tool + "\x00" + variant }

func NewMulti(implementations ...Adapter) *Multi {
	m := &Multi{adapters: make(map[string]Adapter), catalogs: make(map[Adapter][]CatalogEntry, len(implementations))}
	// Every adapter's CLI is asked once, all at the same time, so the cost is the slowest one, not the sum.
	found := make([][]CatalogEntry, len(implementations))
	var wait sync.WaitGroup
	for i, implementation := range implementations {
		wait.Add(1)
		go func() {
			defer wait.Done()
			found[i] = implementation.Catalog()
		}()
	}
	wait.Wait()
	for i, implementation := range implementations {
		m.catalogs[implementation] = found[i]
	}
	for _, implementation := range implementations {
		// Launcher discovery can be inconclusive outside a mirrored Docker
		// checkout. Keep the adapter registered so the checkout preflight can
		// make the final spawn decision.
		if known, ok := implementation.(interface{ CatalogIdentity() (string, string) }); ok {
			tool, variant := known.CatalogIdentity()
			m.adapters[adapterKey(tool, variant)] = implementation
			continue
		}
		for _, item := range m.catalogs[implementation] {
			m.adapters[adapterKey(item.Tool, item.Environment.Variant)] = implementation
		}
	}
	return m
}

// implementationFor is the adapter that runs a spec.
func (m *Multi) implementationFor(spec TurnSpec) Adapter {
	variant := spec.Agent.Variant
	if spec.Agent.LauncherID != "" {
		variant = "native"
		if spec.LauncherContainerized {
			variant = "docker"
		}
	}
	implementation := m.adapters[adapterKey(spec.Agent.Tool, variant)]
	if implementation == nil && spec.Agent.Variant == "" {
		implementation = m.adapters[adapterKey(spec.Agent.Tool, "native")]
	}
	return implementation
}

func (m *Multi) Start(ctx context.Context, spec TurnSpec) (Turn, error) {
	implementation := m.implementationFor(spec)
	if implementation == nil {
		return nil, fmt.Errorf("no adapter for tool %q", spec.Agent.Tool)
	}
	return implementation.Start(ctx, spec)
}

// HasWarm says whether any adapter of the Multi can keep a process warm (a Multi has the warm methods whatever it holds).
func (m *Multi) HasWarm() bool {
	for _, implementation := range m.adapters {
		if _, ok := implementation.(WarmAdapter); ok {
			return true
		}
	}
	return false
}

// WarmFingerprint and StartWarm make a Multi a WarmAdapter for the tools whose adapter is one; for any other they say ErrNoWarm.
func (m *Multi) WarmFingerprint(spec TurnSpec) (string, error) {
	if warm, ok := m.implementationFor(spec).(WarmAdapter); ok {
		return warm.WarmFingerprint(spec)
	}
	return "", ErrNoWarm
}

func (m *Multi) StartWarm(ctx context.Context, spec TurnSpec) (WarmSession, error) {
	if warm, ok := m.implementationFor(spec).(WarmAdapter); ok {
		return warm.StartWarm(ctx, spec)
	}
	return nil, ErrNoWarm
}

// ForgetAgent deletes what each adapter keeps on disk for an agent that carries its key (a generated MCP config), for those that keep any.
func (m *Multi) ForgetAgent(agentID string) {
	seen := map[Adapter]bool{}
	for _, implementation := range m.adapters {
		if seen[implementation] {
			continue
		}
		seen[implementation] = true
		if forgetter, ok := implementation.(interface{ ForgetAgent(string) }); ok {
			forgetter.ForgetAgent(agentID)
		}
	}
}

// SweepKeys asks each adapter that keeps generated configs to delete the ones an older version wrote with a key in them.
func (m *Multi) SweepKeys() {
	seen := map[Adapter]bool{}
	for _, implementation := range m.adapters {
		if seen[implementation] {
			continue
		}
		seen[implementation] = true
		if sweeper, ok := implementation.(interface{ SweepKeys() }); ok {
			sweeper.SweepKeys()
		}
	}
}

func (m *Multi) Catalog() []CatalogEntry {
	var items []CatalogEntry
	for _, implementation := range m.adapters {
		items = append(items, m.catalogs[implementation]...)
	}
	return items
}

func (m *Multi) Capabilities(version string, environment Environment) Capabilities {
	for _, implementation := range m.adapters {
		for _, item := range m.catalogs[implementation] {
			if item.Version == version && item.Environment == environment {
				return implementation.Capabilities(version, environment)
			}
		}
	}
	return Capabilities{}
}

// CapabilitiesFor uses a known launcher identity after the supervisor probes
// it from a mapped checkout. Docker catalogs can be empty at process startup
// when the supervisor's own cwd is not mirrored into the container.
func (m *Multi) CapabilitiesFor(tool, variant, version string, environment Environment) Capabilities {
	implementation := m.adapters[adapterKey(tool, variant)]
	if implementation == nil {
		return Capabilities{}
	}
	return implementation.Capabilities(version, environment)
}

// CanKeepAlive says whether the adapter that runs this spec's tool can run a keep-alive turn.
func (m *Multi) CanKeepAlive(spec TurnSpec) bool {
	implementation := m.implementationFor(spec)
	if implementation == nil {
		return false
	}
	keeper, ok := implementation.(KeepAliver)
	return ok && keeper.KeepAliveTool() == spec.Agent.Tool
}
