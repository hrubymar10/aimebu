package server

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

const (
	agentSessionOriginMCP     = "mcp"
	agentSessionOriginWrapper = "wrapper"
	agentSessionFieldMax      = 4096
)

func normalizeAgentSessionHint(hint *types.AgentSession) (*types.AgentSession, error) {
	if hint == nil {
		return nil, nil
	}
	cp := *hint
	cp.HarnessSessionID = strings.TrimSpace(cp.HarnessSessionID)
	cp.ResumeCommand = strings.TrimSpace(cp.ResumeCommand)
	cp.CWD = strings.TrimSpace(cp.CWD)
	for field, value := range map[string]string{
		"harness_session_id": cp.HarnessSessionID,
		"resume_command":     cp.ResumeCommand,
		"cwd":                cp.CWD,
	} {
		if len(value) > agentSessionFieldMax {
			return nil, fmt.Errorf("session.%s exceeds %d bytes", field, agentSessionFieldMax)
		}
	}
	if cp.HarnessSessionID == "" && cp.ResumeCommand == "" {
		return nil, fmt.Errorf("session requires harness_session_id or resume_command")
	}
	return &cp, nil
}

func (s *store) upsertAgentSessionLocked(agent *types.Agent, origin string, hint *types.AgentSession) error {
	if agent == nil || agent.ID == "" || hint == nil {
		return nil
	}
	nowTime := time.Now().UTC()
	registeredAt := nowTime
	if existing := s.agentSessions[agent.ID]; existing != nil && !existing.RegisteredAt.IsZero() {
		registeredAt = existing.RegisteredAt
	}
	cwd := hint.CWD
	if cwd == "" && agent.Meta != nil {
		cwd = agent.Meta["cwd"]
	}
	sess := &types.AgentSession{
		FullID:           agent.ID,
		Origin:           origin,
		Harness:          agent.Harness,
		Model:            agent.Model,
		Project:          agent.Project,
		CWD:              cwd,
		HarnessSessionID: hint.HarnessSessionID,
		ResumeCommand:    hint.ResumeCommand,
		RegisteredAt:     registeredAt,
		LastSeen:         nowTime,
	}
	s.agentSessions[agent.ID] = sess
	return s.persistAgentSessionSQLiteLocked(sess)
}

func (s *store) touchAgentSessionLocked(fullID string) {
	if sess := s.agentSessions[fullID]; sess != nil {
		sess.LastSeen = time.Now().UTC()
		if err := s.persistAgentSessionSQLiteLocked(sess); err != nil {
			// Session hints are best-effort metadata; callers should not fail
			// liveness/register paths when this write is unavailable.
			return
		}
	}
}

func (s *store) persistAgentSessionSQLiteLocked(sess *types.AgentSession) error {
	if s.db == nil || sess == nil || sess.FullID == "" {
		return nil
	}
	data, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO agent_sessions(full_id, data) VALUES(?, ?) ON CONFLICT(full_id) DO UPDATE SET data=excluded.data`, sess.FullID, string(data))
	return err
}

func (s *store) listAgentSessions() []types.AgentSession {
	s.mu.RLock()
	out := make([]types.AgentSession, 0, len(s.agentSessions))
	for _, sess := range s.agentSessions {
		if sess == nil {
			continue
		}
		out = append(out, *sess)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].FullID < out[j].FullID
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}
