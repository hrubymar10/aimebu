package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

func (s *Store) InboxKind(ctx context.Context, id string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM inbox_item WHERE id=?`, id).Scan(&kind)
	return kind, err
}

func (s *Store) DecideUsageLimit(ctx context.Context, itemID, actorID, key, choice string, rev int) (RecoveryDecision, bool, error) {
	digest := sha256.Sum256([]byte(itemID + "\x00" + choice + "\x00" + strconv.Itoa(rev)))
	result, reused, err := s.WithRequestKey(ctx, RequestKey{ActorKind: "user", ActorID: actorID,
		Kind: "recovery_decision", Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *Tx) (string, error) {
		if err := tx.advanceUsageDays(time.Now()); err != nil {
			return "", err
		}
		var projectID, roomID, requesterID, state, kind string
		var currentRev int
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,room_id,requester_id,state,kind,rev FROM inbox_item WHERE id=?`, itemID).
			Scan(&projectID, &roomID, &requesterID, &state, &kind, &currentRev)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrForbidden
		}
		if err != nil {
			return "", err
		}
		if kind != "usage_limit" {
			return "", ErrForbidden
		}
		if err := tx.roomLifecyclePermission(projectID, actorID); err != nil {
			return "", err
		}
		if currentRev != rev {
			return "", ErrStaleInboxRev
		}
		if state != "open" {
			return "", ErrAlreadyResolved
		}
		parts := strings.SplitN(requesterID, ":", 3)
		if len(parts) != 3 || parts[0] != "project" {
			return "", ErrInvalidTransition
		}
		scopeKind, scopeID, dayKey := parts[0], parts[1], parts[2]
		var continued, paused bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT continued,paused FROM usage_limit_day
			WHERE scope_kind=? AND scope_id=? AND day_start=?`, scopeKind, scopeID, dayKey).
			Scan(&continued, &paused); err != nil {
			return "", err
		}
		if !paused || choice != "continue" && choice != "abandon" || choice == "continue" && continued {
			return "", ErrInvalidTransition
		}
		if choice == "continue" {
			limit, err := tx.usageLimit(scopeKind, scopeID, time.Now())
			if err != nil {
				return "", err
			}
			var cost *float64
			var turns *int64
			if limit.DailyCostCap != nil && limit.DailyCost >= *limit.DailyCostCap {
				raised := math.Max(*limit.DailyCostCap*2, limit.DailyCost*2)
				cost = &raised
			}
			if limit.DailyTurnCap != nil && limit.DailyTurns >= *limit.DailyTurnCap {
				raised := max(*limit.DailyTurnCap*2, limit.DailyTurns*2)
				turns = &raised
			}
			if _, err := tx.Exec(`UPDATE usage_limit_day SET continued=1,limited=0,paused=0,
				override_cost=?,override_turns=? WHERE scope_kind=? AND scope_id=? AND day_start=?`,
				cost, turns, scopeKind, scopeID, dayKey); err != nil {
				return "", err
			}
			if _, err := tx.Exec(`UPDATE `+scopeKind+` SET gate=CASE WHEN control_desired='active' THEN 'open' ELSE 'paused' END,
				gate_rev=gate_rev+1 WHERE id=?`, scopeID); err != nil {
				return "", err
			}
		} else {
			if _, err := tx.Exec(`UPDATE usage_limit_day SET abandoned=1 WHERE scope_kind=? AND scope_id=? AND day_start=?`,
				scopeKind, scopeID, dayKey); err != nil {
				return "", err
			}
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=?`,
			actorID, timestamp(), itemID); err != nil {
			return "", err
		}
		if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
			Body: "Daily usage limit: " + choice + " selected by owner/operator"}); err != nil {
			return "", err
		}
		if _, err := tx.AppendEvent(Event{Kind: "usage_limit_decided", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"item_id": itemID, "choice": choice, "actor_id": actorID})}); err != nil {
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
