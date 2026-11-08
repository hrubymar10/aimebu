package server

import (
	"net/http"
	"time"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

// keepAliveScopeTTL (protocol.KeepAliveScopeTTL) bounds a ping's scope on the server: a supervisor that lifts it always does so once the ping is
// confirmed gone, but one that died or lost its connection cannot, and the agent's real tool calls must not be refused for ever. A supervisor that
// holds an unconfirmed ping in quarantine renews the scope (the same "on" frame again extends the lease) every protocol.KeepAliveScopeRenewEvery, so
// the quarantine is not bound by this. A scope is not dropped when a connection comes or goes: a supervisor that reconnects while a ping runs must
// not leave that ping free to act.
const keepAliveScopeTTL = protocol.KeepAliveScopeTTL

// keepAliveScopeLease says which machine's supervisor scoped an agent, and until when.
type keepAliveScopeLease struct {
	machineID string
	gen       string // the ping the scope is for (see protocol.Frame.ScopeGen); empty from a supervisor that does not say
	until     time.Time
}

// scopeKeepAlive begins or lifts the scope of a ping of an agent. While it is on, requireAgentTool refuses every call made with the agent's key
// (403 keep_alive_scope): the cache ping is only asked in words not to use the toolbox, and a model that does anyway must not post, change or spawn
// anything. Only the machine that runs the agent may scope it.
func (s *Server) scopeKeepAlive(agentID, machineID, gen string, on bool) {
	s.kaScopeMu.Lock()
	defer s.kaScopeMu.Unlock()
	if !on {
		// A lift that names another ping than the one scoped now is a late one (an owed lift of an older ping): it does not clear the live scope.
		if lease, ok := s.kaScope[agentID]; ok && lease.machineID == machineID && (gen == "" || lease.gen == "" || lease.gen == gen) {
			delete(s.kaScope, agentID)
		}
		return
	}
	if s.kaScope == nil {
		s.kaScope = map[string]keepAliveScopeLease{}
	}
	s.kaScope[agentID] = keepAliveScopeLease{machineID: machineID, gen: gen, until: time.Now().Add(keepAliveScopeTTL)}
}

// keepAliveScoped says a ping of the agent is running (and its toolbox is refused).
func (s *Server) keepAliveScoped(agentID string) bool {
	s.kaScopeMu.Lock()
	defer s.kaScopeMu.Unlock()
	lease, ok := s.kaScope[agentID]
	if !ok {
		return false
	}
	if time.Now().After(lease.until) {
		delete(s.kaScope, agentID)
		return false
	}
	return true
}

// handleKeepAliveScope applies a keep_alive_scope frame and answers it.
func (s *Server) handleKeepAliveScope(c *machineConn, frame protocol.Frame, runsHere func(agentID string) bool) {
	if frame.AgentID != "" && runsHere(frame.AgentID) {
		if frame.ScopeOn {
			// A ping is about to run on this machine: whatever its last report said, it has something running now (the next report says exactly what).
			// The frame's counter value comes after the ping is in the supervisor's count, so no earlier report can overwrite this. If the count cannot
			// be saved the ping is not answered, so it does not run: the server's knowledge must never be behind a running process.
			if !s.applyWarmCount(c, frame.WarmSeq, 1, true) {
				return
			}
		}
		s.scopeKeepAlive(frame.AgentID, c.id, frame.ScopeGen, frame.ScopeOn)
		_ = c.send(protocol.Frame{V: protocol.Version, Type: protocol.KeepAliveScopeAckFrame, ReqID: frame.ReqID})
	}
}

func refuseKeepAliveScope(w http.ResponseWriter) {
	apiError(w, http.StatusForbidden, "keep_alive_scope", "this is a cache keep-alive, not work: the toolbox is not available")
}
