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
	// RegisteredAt is when this registration's session record began: a new holder of the name does not inherit the previous holder's.
	if existing := s.agentSessions[agent.ID]; existing != nil && !existing.RegisteredAt.IsZero() && existing.RegistrationID == agent.RegistrationID {
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
		RegistrationID:   agent.RegistrationID,
		RegisteredAt:     registeredAt,
		LastSeen:         nowTime,
	}
	s.agentSessions[agent.ID] = sess
	if err := s.recordAgentSessionHistoryLocked(sess, nowTime); err != nil {
		return err
	}
	return s.persistAgentSessionSQLiteLocked(sess)
}

// recordAgentSessionHistoryLocked adds the session to the agent identity's history: a session id not seen for this identity appends a row,
// one already there only has its details and last_seen refreshed (so a repeated push is idempotent, and resuming an older session updates
// that session's row); firstSeen is when a new row's session was first seen. A hint with no session id (a resume command alone) has nothing to key a row by and leaves the history as it is.
func (s *store) recordAgentSessionHistoryLocked(sess *types.AgentSession, firstSeen time.Time) error {
	if sess == nil || sess.FullID == "" || sess.HarnessSessionID == "" {
		return nil
	}
	key := agentSessionHistoryKey{sess.FullID, sess.HarnessSessionID}
	entry := s.agentSessionHistoryIndex[key]
	if entry == nil {
		entry = &types.AgentSessionHistoryEntry{FullID: sess.FullID, HarnessSessionID: sess.HarnessSessionID, RegisteredAt: firstSeen}
		s.agentSessionHistory[sess.FullID] = append(s.agentSessionHistory[sess.FullID], entry)
		s.agentSessionHistoryIndex[key] = entry
	}
	entry.Harness, entry.Model, entry.Project, entry.Origin, entry.LastSeen = sess.Harness, sess.Model, sess.Project, sess.Origin, sess.LastSeen
	if sess.CWD != "" {
		entry.CWD = sess.CWD
	}
	if s.db == nil {
		if entry.ID == 0 {
			s.nextSessionHistory++
			entry.ID = s.nextSessionHistory
		}
		return nil
	}
	return s.db.QueryRow(`INSERT INTO agent_session_history(full_id, harness_session_id, harness, model, project, cwd, origin, registered_at, last_seen)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(full_id, harness_session_id) DO UPDATE SET harness=excluded.harness, model=excluded.model, project=excluded.project,
			cwd=excluded.cwd, origin=excluded.origin, last_seen=excluded.last_seen
		RETURNING id`, entry.FullID, entry.HarnessSessionID, entry.Harness, entry.Model, entry.Project, entry.CWD, entry.Origin,
		entry.RegisteredAt.Format(time.RFC3339Nano), entry.LastSeen.Format(time.RFC3339Nano)).Scan(&entry.ID)
}

// listAgentSessionHistory returns an identity's sessions, most recently seen first.
func (s *store) listAgentSessionHistory(fullID string) []types.AgentSessionHistoryEntry {
	s.mu.RLock()
	out := make([]types.AgentSessionHistoryEntry, 0, len(s.agentSessionHistory[fullID]))
	for _, entry := range s.agentSessionHistory[fullID] {
		out = append(out, *entry)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].ID > out[j].ID
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

func (s *store) touchAgentSessionLocked(fullID string) {
	if sess := s.agentSessions[fullID]; sess != nil && sessionIsCurrentLocked(s.agents[fullID], sess) {
		sess.LastSeen = time.Now().UTC()
		s.touchAgentSessionHistoryLocked(sess)
		if err := s.persistAgentSessionSQLiteLocked(sess); err != nil {
			// Session hints are best-effort metadata; callers should not fail
			// liveness/register paths when this write is unavailable.
			return
		}
	}
}

// touchAgentSessionHistoryLocked keeps the current session's history row as fresh as the latest record, so "most recently seen" tracks
// liveness as well as pushes. Best-effort like the touch itself.
func (s *store) touchAgentSessionHistoryLocked(sess *types.AgentSession) {
	if sess.HarnessSessionID == "" {
		return
	}
	if entry := s.agentSessionHistoryIndex[agentSessionHistoryKey{sess.FullID, sess.HarnessSessionID}]; entry != nil {
		entry.LastSeen = sess.LastSeen
		if s.db != nil {
			_, _ = s.db.Exec(`UPDATE agent_session_history SET last_seen=? WHERE full_id=? AND harness_session_id=?`,
				sess.LastSeen.Format(time.RFC3339Nano), sess.FullID, sess.HarnessSessionID)
		}
	}
}

// agentSessionHistoryKey names one history row: a harness session of one agent identity.
type agentSessionHistoryKey struct{ fullID, harnessSessionID string }

// sessionIsCurrentLocked says whether the latest session record of an agent identity was pushed under the registration that holds the
// identity now. agent_sessions is keyed by the identity alone and outlives deregistration, so a name that a new instance takes over still has
// its previous holder's record until the new one pushes its own: that record must neither be attributed to the new holder's messages nor be
// kept fresh by its liveness. Registrations are told apart by id, not by time, because two holders can follow each other within one clock
// tick. Records and agents from before the id existed all carry none, and match.
func sessionIsCurrentLocked(agent *types.Agent, sess *types.AgentSession) bool {
	return agent != nil && sess != nil && sess.RegistrationID == agent.RegistrationID
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
