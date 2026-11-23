package store

import (
	"context"
	"sort"
	"time"
)

// A switch quarantine holds new turns of an agent after an account switch the owner forced past its warm process: the host's supervisor could not
// confirm that process ended, so it may still hold the old login, and a new turn on its session must wait until it is confirmed gone (the supervisor
// reports warm_ended, which lifts it) or the quarantine's time is up. It lives in memory only: a restarted server has a supervisor that reconnects and
// reports what it runs, and the supervisor itself keeps a session with an unconfirmed process from starting another.

// QuarantineAgent holds new turns of the agent until ReleaseQuarantine or until.
func (s *Store) QuarantineAgent(agentID string, until time.Time) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	if s.quarantine == nil {
		s.quarantine = map[string]time.Time{}
	}
	s.quarantine[agentID] = until
}

// ReleaseQuarantine lifts the agent's quarantine and says whether it had one in force.
func (s *Store) ReleaseQuarantine(agentID string) bool {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	until, ok := s.quarantine[agentID]
	delete(s.quarantine, agentID)
	return ok && time.Now().Before(until)
}

// AgentQuarantined says whether the agent's quarantine is in force.
func (s *Store) AgentQuarantined(agentID string) bool {
	s.holdMu.RLock()
	defer s.holdMu.RUnlock()
	until, ok := s.quarantine[agentID]
	return ok && time.Now().Before(until)
}

// QuarantinedAgents lists the agents whose quarantine is in force, sorted.
func (s *Store) QuarantinedAgents() []string {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	now := time.Now()
	out := []string{}
	for agentID, until := range s.quarantine {
		if now.Before(until) {
			out = append(out, agentID)
		} else {
			delete(s.quarantine, agentID)
		}
	}
	sort.Strings(out)
	return out
}

// RecordSwitchQuarantine puts the agent's quarantine on its record (an agent_switch_quarantined event in its project).
func (s *Store) RecordSwitchQuarantine(ctx context.Context, agentID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, agentID).Scan(&projectID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "agent_switch_quarantined", ProjectID: projectID, AgentID: agentID,
			PayloadJSON: payloadJSON(map[string]string{"reason": "an account switch went past its warm process"})})
		return err
	})
}

// AgentLabel is an agent as a person reads it: slug@project.
type AgentLabel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// AgentLabels names agents as slug@project, in the order given; an id with no agent keeps the id as its label.
func (s *Store) AgentLabels(ctx context.Context, ids []string) ([]AgentLabel, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	out := make([]AgentLabel, 0, len(ids))
	for _, id := range ids {
		label := id
		var slug, project string
		err := s.db.QueryRowContext(ctx, `SELECT a.slug,p.name FROM agent a JOIN project p ON p.id=a.project_id WHERE a.id=?`, id).Scan(&slug, &project)
		if err == nil {
			label = slug + "@" + project
		}
		out = append(out, AgentLabel{ID: id, Label: label})
	}
	return out, nil
}
