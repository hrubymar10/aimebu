package server

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

// agentToolDM is the dm_agent tool: an active lead starts (or continues) a private chat with any agent, in any project it may reach.
// Authorization is the store's, checked in the same transaction as the write.
func (s *Server) agentToolDM(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	if !activeLead(w, caller) {
		return
	}
	key, ok := toolRequestKey(w, r)
	if !ok {
		return
	}
	var req struct {
		TargetAgentID string `json:"target_agent_id"`
		Body          string `json:"body"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.TargetAgentID == "" || len(req.TargetAgentID) > 100 {
		apiError(w, 400, "invalid_request", "target_agent_id is required")
		return
	}
	if err := store.ValidAgentDMBody(req.Body); err != nil {
		apiError(w, 400, "invalid_request", err.Error())
		return
	}
	result, _, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "agent", ActorID: caller.ID,
		Kind: "dm_agent", Key: key, Digest: toolDigest(req)}, func(tx *store.Tx) (string, error) {
		sent, err := tx.DMAgentAsTool(caller.ID, req.TargetAgentID, req.Body)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(sent)
		return string(data), err
	})
	if err != nil {
		toolDMError(w, err)
		return
	}
	var sent store.AgentDMResult
	if err := json.Unmarshal([]byte(result), &sent); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, sent)
}

// agentToolDMHuman is the dm_human tool: any active agent, a worker as much as a lead, starts (or continues) a private chat with a person of its
// own project. Who may is the store's, checked in the same transaction as the write.
func (s *Server) agentToolDMHuman(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	key, ok := toolRequestKey(w, r)
	if !ok {
		return
	}
	var req struct {
		TargetUser string `json:"target_user"`
		Body       string `json:"body"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.TargetUser == "" || len(req.TargetUser) > 100 {
		apiError(w, 400, "invalid_request", "target_user is required: a person's login")
		return
	}
	if err := store.ValidAgentDMBody(req.Body); err != nil {
		apiError(w, 400, "invalid_request", err.Error())
		return
	}
	result, _, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: "agent", ActorID: caller.ID,
		Kind: "dm_human", Key: key, Digest: toolDigest(req)}, func(tx *store.Tx) (string, error) {
		sent, err := tx.DMHumanAsTool(caller.ID, req.TargetUser, req.Body)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(sent)
		return string(data), err
	})
	if err != nil {
		humanDMError(w, err)
		return
	}
	var sent store.HumanDMResult
	if err := json.Unmarshal([]byte(result), &sent); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, sent)
}

func humanDMError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "you may message only a person who is a member of your own project (give their login), and you must be active")
	case errors.Is(err, store.ErrProjectArchived):
		apiError(w, 409, "project_archived", "your project is archived")
	case errors.Is(err, store.ErrHumanDMCap):
		apiError(w, 429, "human_dm_cap", "you wrote as many messages to this person this hour as allowed; wait for their answer")
	case errors.Is(err, store.ErrHumanDMFanOutCap):
		apiError(w, 429, "human_dm_fanout_cap", "you opened as many new chats with people this hour as allowed")
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "idempotency_conflict", "key reused with different request")
	case errors.Is(err, store.ErrInvalidTransition):
		apiError(w, 409, "invalid_request", "this chat is closed and takes no messages")
	default:
		fail(w, err)
	}
}

func toolDMError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "you may not message that agent: the people behind this turn must have access to its project, and it must exist")
	case errors.Is(err, store.ErrAgentUnavailable):
		apiError(w, 409, "agent_unavailable", "that agent is stopped, not accepting work, or removed")
	case errors.Is(err, store.ErrProjectArchived):
		apiError(w, 409, "project_archived", "a project in this chat is archived")
	case errors.Is(err, store.ErrDMTurnCap):
		apiError(w, 429, "dm_turn_cap", "this chat used its agent turns for the hour; try again later or ask a person to write in it")
	case errors.Is(err, store.ErrDMFanOutCap):
		apiError(w, 429, "dm_fanout_cap", "you opened as many new chats this hour as allowed")
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "idempotency_conflict", "key reused with different request")
	case errors.Is(err, store.ErrInvalidTransition):
		apiError(w, 400, "invalid_request", "you cannot message yourself, and a closed chat takes no messages")
	default:
		fail(w, err)
	}
}

// agentDMs lists the chats between agents the person has joined, and those they could join (title and activity only).
func (s *Server) agentDMs(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.Store.AgentDMsForUser(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	offers, err := s.Store.AgentDMOffers(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"project_id": store.AgentDMProjectID, "rooms": rooms, "joinable": offers})
}

func (s *Server) joinAgentDM(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.JoinAgentDM(r.Context(), userID(r), r.PathValue("room")); err != nil {
		agentDMPeopleError(w, err)
		return
	}
	s.writeAgentDMRoom(w, r, 200)
}

func (s *Server) leaveAgentDM(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.LeaveAgentDM(r.Context(), userID(r), r.PathValue("room")); err != nil {
		agentDMPeopleError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"room_id": r.PathValue("room")})
}

func (s *Server) writeAgentDMRoom(w http.ResponseWriter, r *http.Request, status int) {
	room, err := s.Store.RoomForUserProject(r.Context(), store.AgentDMProjectID, userID(r), r.PathValue("room"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, status, room)
}

// agentDMPeopleError answers a join or leave the same way whether the chat does not exist or is none of the person's business.
func agentDMPeopleError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrForbidden) || errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "no such chat")
		return
	}
	fail(w, err)
}
