package server

import (
	"context"
	"fmt"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

// grantAgentKeys is the new keys a supervisor gets in hello_ack: one for each agent it should be running and holds no key for. A supervisor keeps
// keys in memory only, so one that restarted holds none, and its agents would stop being able to reach the bus; issuing a key replaces the agent's
// stored hash, so the key the old process held no longer authenticates. A supervisor that does not say it takes keys (an older one, which keeps
// them on disk) gets none.
func (s *Server) grantAgentKeys(ctx context.Context, machine *machineConn, held []string) ([]protocol.AgentKeyGrant, error) {
	if !machine.supportsAgentKeys {
		return nil, nil
	}
	// A spawn issues its key under spawnMu and keeps it there until the supervisor answers. Looking and issuing under the same lock means a key is never
	// replaced after a spawn or a switch has already sent its own (the agent would keep a dead key until it restarted), and an agent whose spawn key is still
	// waiting for its answer is left to that spawn.
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	have := make(map[string]bool, len(held))
	for _, id := range held {
		have[id] = true
	}
	candidates, err := s.Store.AgentsToKey(ctx, machine.id)
	if err != nil {
		return nil, err
	}
	var grants []protocol.AgentKeyGrant
	for _, candidate := range candidates {
		if have[candidate.AgentID] || s.spawnKeys[fmt.Sprintf("%s/%d", candidate.AgentID, candidate.Generation)] != "" {
			continue
		}
		key, err := s.Auth.IssueAgentKey(ctx, candidate.AgentID)
		if err != nil {
			return nil, err
		}
		grants = append(grants, protocol.AgentKeyGrant{AgentID: candidate.AgentID, Generation: candidate.Generation, Key: key})
	}
	return grants, nil
}
