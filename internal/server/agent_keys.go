package server

import (
	"context"
	"fmt"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
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
		if s.connRetiring(machine) {
			continue
		}
		blocked, err := s.handoverBlocked(ctx, machine.id, candidate.AgentID)
		if err != nil {
			return nil, err
		}
		if blocked {
			continue
		}
		key, err := s.Auth.IssueAgentKey(ctx, candidate.AgentID)
		if err != nil {
			return nil, err
		}
		grants = append(grants, protocol.AgentKeyGrant{AgentID: candidate.AgentID, Generation: candidate.Generation, Key: key})
		machine.keysMu.Lock()
		machine.keys[candidate.AgentID] = true
		machine.keysMu.Unlock()
	}
	return grants, nil
}

// grantHandoverKey sends a key on the active connection before its first delivery for an agent released by retiring instances. Only an instance
// that says it handles the standalone frame gets one. An older build that takes keys only in hello_ack must reconnect before delivery.
func (s *Server) grantHandoverKey(ctx context.Context, machine *machineConn, agentID string, generation int) error {
	if !machine.supportsAgentKeys {
		return nil
	}
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	machine.keysMu.Lock()
	defer machine.keysMu.Unlock()
	if machine.keys[agentID] {
		return nil
	}
	if s.spawnKeys[fmt.Sprintf("%s/%d", agentID, generation)] != "" {
		return ErrMachineUnavailable
	}
	if !machine.supportsAgentKeysFrame {
		_ = machine.conn.Close(websocket.StatusTryAgainLater, "agent key requires reconnect")
		machine.stop()
		return ErrMachineUnavailable
	}
	key, err := s.Auth.IssueAgentKey(ctx, agentID)
	if err != nil {
		return err
	}
	if err := machine.send(protocol.Frame{V: protocol.Version, Type: protocol.AgentKeys, AgentKeys: []protocol.AgentKeyGrant{{AgentID: agentID, Generation: generation, Key: key}}}); err != nil {
		return err
	}
	machine.keys[agentID] = true
	return nil
}
