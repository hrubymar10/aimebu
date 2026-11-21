package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

var resetTimestamp = regexp.MustCompile(`(?i)(?:resets?|reset_at|resets_at|retry|try again)[^0-9]{0,30}(\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[Zz]|[+-]\d{2}:\d{2}))`)
var resetDelay = regexp.MustCompile(`(?i)\b(?:resets?|retry|try again)\s+in\s+(\d+)\s*(minutes?|hours?)\b`)

// A provider limit is a known launcher failure only when the provider's own
// wording appears in its process evidence. A generic HTTP 429 or a user
// message mentioning a limit cannot bypass ordinary recovery.
func providerLimitEvidence(tool, stderr, errorEvent string, now time.Time) (string, bool) {
	if tool != "codex" && tool != "claude" && tool != "pi" && tool != "opencode" && tool != "vibe" {
		return "", false
	}
	evidence := stderr + "\n" + errorEvent
	providerWording := tool == "pi" || tool == "opencode" || tool == "vibe"
	if providerWording {
		// Only the provider error the harness reported counts: its stderr is
		// launcher noise, and a plain rate limit is transient, not a cap that
		// gates the agent until a reset.
		evidence = errorEvent
	}
	lower := strings.ToLower(evidence)
	if tool == "codex" && !strings.Contains(lower, "usage limit") ||
		tool == "claude" && (!strings.Contains(lower, "limit reached") || strings.Contains(lower, "rate limit reached")) ||
		providerWording && !providerCapWording(lower) {
		return "", false
	}
	for _, match := range resetTimestamp.FindAllStringSubmatch(evidence, -1) {
		value := match[1]
		value = strings.Replace(value, " ", "T", 1)
		value = strings.Replace(value, "t", "T", 1)
		if strings.HasSuffix(value, "z") {
			value = strings.TrimSuffix(value, "z") + "Z"
		}
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UTC().Format("2006-01-02T15:04:05.000000000Z"), true
		}
	}
	if match := resetDelay.FindStringSubmatch(evidence); match != nil {
		count, _ := strconv.Atoi(match[1])
		if count > 0 && count <= 24*60 {
			delay := time.Duration(count) * time.Minute
			if strings.HasPrefix(strings.ToLower(match[2]), "hour") {
				delay = time.Duration(count) * time.Hour
			}
			return now.Add(delay).UTC().Format("2006-01-02T15:04:05.000000000Z"), true
		}
	}
	return "", true
}

// providerCapWording recognises the wording providers behind Pi, OpenCode and Vibe
// use for a plan or quota cap, the same kind the harness itself refuses to
// retry. A bare "rate limit" or HTTP 429 is deliberately not enough.
func providerCapWording(lower string) bool {
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate_limit") {
		return false
	}
	for _, phrase := range []string{"usage limit", "usage_limit", "weekly limit", "hourly limit", "monthly limit", "daily limit"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func (tx *Tx) recordProviderLimit(turnID, agentID, projectID, roomID, tool, resetAt string) error {
	if err := tx.requeueTurnNotes(turnID, agentID, "failed"); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,reset_at,tool) VALUES (?,?,?,?,?)
		ON CONFLICT(agent_id) DO UPDATE SET turn_id=excluded.turn_id,detected_at=excluded.detected_at,
		reset_at=excluded.reset_at,tool=excluded.tool`, agentID, turnID, timestamp(), nullable(resetAt), tool); err != nil {
		return err
	}
	var existing int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM inbox_item WHERE agent_id=? AND kind='usage_info' AND requester_kind='system' AND requester_id='provider_limit' AND state='open'`, agentID).Scan(&existing); err != nil {
		return err
	}
	if existing == 0 {
		id, err := newID("inb")
		if err != nil {
			return err
		}
		message := fmt.Sprintf("%s reached its provider usage limit. New turns are paused", tool)
		if resetAt != "" {
			message += " until " + resetAt
		} else {
			message += " until the agent is switched or stopped and reactivated"
		}
		message += ". Its request is queued for another attempt; check the state before acting if the previous turn may have made changes."
		options, err := json.Marshal(inboxAsk{Text: message, Options: []string{"Retry now"}})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
			VALUES (?,?,?,?,?,'system','provider_limit','usage_info',?,?)`, id, projectID, roomID, turnID, agentID, string(options), timestamp()); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "usage_info"})}); err != nil {
			return err
		}
	}
	_, err := tx.AppendEvent(Event{Kind: "provider_limit_detected", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID,
		PayloadJSON: payloadJSON(map[string]string{"reset_at": resetAt})})
	return err
}

func (tx *Tx) clearProviderLimit(agentID string) error {
	var projectID, roomID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id FROM provider_limit p
		JOIN agent a ON a.id=p.agent_id JOIN turn t ON t.id=p.turn_id WHERE a.id=?`, agentID).Scan(&projectID, &roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM provider_limit WHERE agent_id=?`, agentID); err != nil {
		return err
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id FROM inbox_item WHERE agent_id=? AND kind='usage_info' AND requester_kind='system' AND requester_id='provider_limit' AND state='open'`, agentID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_at=? WHERE id=?`, timestamp(), id); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id})}); err != nil {
			return err
		}
	}
	_, err = tx.AppendEvent(Event{Kind: "provider_limit_cleared", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
		PayloadJSON: payloadJSON(map[string]string{"agent_id": agentID})})
	return err
}

func (tx *Tx) providerLimitBlocked(agentID, tool string) (bool, error) {
	var resetAt sql.NullString
	var limitedTool string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT reset_at,tool FROM provider_limit WHERE agent_id=?`, agentID).Scan(&resetAt, &limitedTool)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if limitedTool != tool || resetAt.Valid && resetAt.String <= timestamp() {
		return false, tx.clearProviderLimit(agentID)
	}
	return true, nil
}

// AdvanceProviderLimits resolves expired limits even when an agent has no
// queued notes. The dispatcher calls this at a coarse cadence.
func (s *Store) AdvanceProviderLimits(ctx context.Context) error {
	return s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT p.agent_id FROM provider_limit p JOIN agent a ON a.id=p.agent_id
			WHERE (p.reset_at IS NOT NULL AND p.reset_at<=?) OR p.tool<>a.tool ORDER BY p.agent_id`, timestamp())
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := tx.clearProviderLimit(id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DecideProviderLimit(ctx context.Context, itemID, actorID, key, choice string, rev int) (RecoveryDecision, bool, error) {
	if choice != "retry" {
		return RecoveryDecision{}, false, ErrInvalidTransition
	}
	digest := sha256.Sum256([]byte(itemID + "\x00" + choice + "\x00" + strconv.Itoa(rev)))
	result, reused, err := s.WithRequestKey(ctx, RequestKey{ActorKind: "user", ActorID: actorID,
		Kind: "recovery_decision", Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *Tx) (string, error) {
		var projectID, agentID, state, kind, requester string
		var currentRev int
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,COALESCE(agent_id,''),state,kind,requester_id,rev FROM inbox_item WHERE id=?`, itemID).
			Scan(&projectID, &agentID, &state, &kind, &requester, &currentRev)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrForbidden
		}
		if err != nil {
			return "", err
		}
		if err := tx.roomLifecyclePermission(projectID, actorID); err != nil {
			return "", err
		}
		if kind != "usage_info" || requester != "provider_limit" || agentID == "" {
			return "", ErrForbidden
		}
		if currentRev != rev {
			return "", ErrStaleInboxRev
		}
		if state != "open" {
			return "", ErrAlreadyResolved
		}
		if err := tx.clearProviderLimit(agentID); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(RecoveryDecision{ItemID: itemID, Choice: choice, Rev: rev + 1})
		return string(encoded), err
	})
	if err != nil {
		return RecoveryDecision{}, false, err
	}
	var decision RecoveryDecision
	if err := json.Unmarshal([]byte(result), &decision); err != nil {
		return RecoveryDecision{}, false, err
	}
	return decision, reused, nil
}

// ProviderLimitedAgents lists the agents on a machine whose provider limit is in force now (it has no reset time yet, or the reset time has not come):
// they get no turns, and a cache keep-alive ping would only meet the same limit.
func (s *Store) ProviderLimitedAgents(ctx context.Context, machineID string) ([]string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.agent_id FROM provider_limit p JOIN agent a ON a.id=p.agent_id
		WHERE a.machine_id=? AND p.tool=a.tool AND (p.reset_at IS NULL OR p.reset_at>?) ORDER BY p.agent_id`, machineID, timestamp())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
