package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/goccy/go-json"
)

// Pool provenance (A14-1). Every agent records where its harness and model came from (agent.pool_origin): "pool" (a pick from its role's
// default pool), "explicit" (a person's own choice), "approved" (a deviation a person approved) or "unknown" (an agent from before the
// record was kept whose choice the pool did not contain). When a role's pool changes, the running agents of origin "pool" whose choice the
// pool no longer lists are offered to the project's owner and admins as one Inbox item per project, to respawn on the pool or to keep.
const (
	PoolOriginPool     = "pool"
	PoolOriginExplicit = "explicit"
	PoolOriginApproved = "approved"
	PoolOriginUnknown  = "unknown"
)

// PoolChangeRespawn and PoolChangeKeep are the two choices of a pool_change item.
const (
	PoolChangeRespawn = "respawn"
	PoolChangeKeep    = "keep"
)

// PoolChangeOptions are the item's two buttons, the respawn first (the default).
var PoolChangeOptions = []string{"Respawn on the pool model", "Keep the current model"}

// PoolAgent is one agent a pool change would switch: it runs on a choice its role's pool no longer lists.
type PoolAgent struct {
	AgentID    string `json:"agent_id"`
	Slug       string `json:"slug"`
	RoleKey    string `json:"role_key"`
	RoleLabel  string `json:"role_label"`
	Lead       bool   `json:"lead,omitempty"`
	RoomID     string `json:"room_id"`
	RoomTitle  string `json:"room_title"`
	Tool       string `json:"tool"`
	Model      string `json:"model"`
	LauncherID string `json:"launcher_id,omitempty"`
	// Busy says a turn of the agent is running: a respawn waits for it to finish.
	Busy bool `json:"busy,omitempty"`
	// RespawnError is why the last respawn of this agent could not begin, in words ("respawn failed: ..."); empty when none failed.
	RespawnError string `json:"respawn_error,omitempty"`
}

type poolQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// effectivePool is the pool in force for a role key in a project (the project's override, else the role's own) and a signature of it, which
// changes whenever the list or its order does. A role that is gone has an empty pool.
func effectivePool(ctx context.Context, q poolQuerier, projectID, roleKey string) ([]SpawnDefaultChoice, string, error) {
	var roleID, globalJSON, items string
	err := q.QueryRowContext(ctx, `SELECT id,spawn_defaults_json FROM role WHERE role_key=? AND deleted_at IS NULL`, roleKey).Scan(&roleID, &globalJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return []SpawnDefaultChoice{}, "none", nil
	}
	if err != nil {
		return nil, "", err
	}
	items = globalJSON
	err = q.QueryRowContext(ctx, `SELECT items_json FROM project_role_spawn_default WHERE project_id=? AND role_id=?`, projectID, roleID).Scan(&items)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	var pool []SpawnDefaultChoice
	if err := json.Unmarshal([]byte(items), &pool); err != nil {
		return nil, "", err
	}
	if pool == nil {
		pool = []SpawnDefaultChoice{}
	}
	sum := sha256.Sum256([]byte(items))
	return pool, hex.EncodeToString(sum[:8]), nil
}

func poolHas(pool []SpawnDefaultChoice, launcherID, model string) bool {
	return slices.ContainsFunc(pool, func(c SpawnDefaultChoice) bool { return c.LauncherID == launcherID && c.Model == model })
}

// poolAffected lists the agents of a project a pool change leaves on a model the pool no longer lists: desired active, origin "pool", not
// already switching or queued for a respawn, and not kept against the pool now in force. Leaders and the dispatcher count like anyone.
func poolAffected(ctx context.Context, q poolQuerier, projectID string) ([]PoolAgent, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.slug,r.role_key,r.label,r.cap_is_lead,COALESCE(a.launcher_id,''),a.model,a.tool,COALESCE(a.pool_kept_sig,''),
		COALESCE((SELECT m.room_id FROM room_member m JOIN room rm ON rm.id=m.room_id WHERE m.member_id=a.id AND m.member_kind='agent' AND m.left_at IS NULL
			ORDER BY m.joined_at LIMIT 1),''),
		EXISTS(SELECT 1 FROM turn t WHERE t.agent_id=a.id AND t.exec IN ('pending','running'))
		FROM agent a JOIN role r ON r.id=a.role_id
		WHERE a.project_id=? AND a.revoked_at IS NULL AND a.desired='active' AND a.pool_origin='pool' AND a.launcher_id IS NOT NULL
		  AND NOT EXISTS(SELECT 1 FROM agent_switch s WHERE s.agent_id=a.id AND s.state IN ('draining','starting'))
		  AND NOT EXISTS(SELECT 1 FROM pool_respawn p WHERE p.agent_id=a.id)
		ORDER BY a.created_at,a.id`, projectID)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		agent PoolAgent
		kept  string
	}
	var found []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.agent.AgentID, &c.agent.Slug, &c.agent.RoleKey, &c.agent.RoleLabel, &c.agent.Lead, &c.agent.LauncherID, &c.agent.Model,
			&c.agent.Tool, &c.kept, &c.agent.RoomID, &c.agent.Busy); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	type pooled struct {
		items []SpawnDefaultChoice
		sig   string
	}
	pools := map[string]pooled{}
	out := []PoolAgent{}
	for _, c := range found {
		p, ok := pools[c.agent.RoleKey]
		if !ok {
			p.items, p.sig, err = effectivePool(ctx, q, projectID, c.agent.RoleKey)
			if err != nil {
				return nil, err
			}
			pools[c.agent.RoleKey] = p
		}
		if poolHas(p.items, c.agent.LauncherID, c.agent.Model) || c.kept == p.sig {
			continue
		}
		if c.agent.RoomID != "" {
			if err := q.QueryRowContext(ctx, `SELECT title FROM room WHERE id=?`, c.agent.RoomID).Scan(&c.agent.RoomTitle); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		out = append(out, c.agent)
	}
	return out, nil
}

// PoolAffected is the live list of a project's agents a pool change is waiting to switch; the Inbox item shows it.
func (s *Store) PoolAffected(ctx context.Context, projectID string) ([]PoolAgent, error) {
	return poolAffected(ctx, s.readDB, projectID)
}

func poolChangeText(count int) string {
	if count == 1 {
		return "A role's default pool changed: 1 running agent uses a harness or model the pool no longer lists."
	}
	return fmt.Sprintf("A role's default pool changed: %d running agents use a harness or model the pool no longer lists.", count)
}

// syncPoolChange brings a project's pool_change item in line with the agents a pool leaves behind: one open item per project, created when
// there are some, updated (its revision moves, so a decision made on the older list is refused as stale) when the list or the pool changed
// again, withdrawn when none are left. A pool change before a decision therefore never raises a second item.
func (tx *Tx) syncPoolChange(projectID string) error {
	affected, err := poolAffected(tx.ctx, tx.tx, projectID)
	if err != nil {
		return err
	}
	var itemID, roomID string
	var rev int64
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT id,room_id,rev FROM inbox_item WHERE kind='pool_change' AND project_id=? AND state='open'`, projectID).Scan(&itemID, &roomID, &rev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	exists := err == nil
	if len(affected) == 0 {
		if !exists {
			return nil
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='withdrawn',rev=rev+1,resolved_at=? WHERE id=?`, timestamp(), itemID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "resolution": "withdrawn", "reason": "no affected agents left"})})
		return err
	}
	next := inboxAsk{Text: poolChangeText(len(affected)), Options: PoolChangeOptions}
	if exists {
		var options string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(options_json,'') FROM inbox_item WHERE id=?`, itemID).Scan(&options); err != nil {
			return err
		}
		var old inboxAsk
		if options != "" {
			if err := json.Unmarshal([]byte(options), &old); err != nil {
				return err
			}
		}
		for _, a := range affected {
			if reason, ok := old.RespawnFailures[a.AgentID]; ok {
				if next.RespawnFailures == nil {
					next.RespawnFailures = map[string]string{}
				}
				next.RespawnFailures[a.AgentID] = reason
			}
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		// Nothing changed (a role save that did not touch the pool, or a look after a failure was recorded): leave the row and its revision
		// alone, so a card on screen does not go stale and the next decision or respawn is not refused as 409 stale_rev.
		if string(encoded) == options {
			return nil
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET options_json=?,rev=rev+1 WHERE id=?`, string(encoded), itemID); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "inbox_item_updated", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "kind": "pool_change"})})
		return err
	}
	ask, err := json.Marshal(next)
	if err != nil {
		return err
	}
	var homeID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? ORDER BY kind='home' DESC,created_at,id LIMIT 1`, projectID).Scan(&homeID); err != nil {
		return err
	}
	if itemID, err = newID("inb"); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES (?,?,?,'system',?,'pool_change',?,?)`, itemID, projectID, homeID, "pool:"+projectID, string(ask), timestamp()); err != nil {
		return err
	}
	if _, err := tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: homeID,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "kind": "pool_change", "agents": strconv.Itoa(len(affected))})}); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "pool_change_detected", ProjectID: projectID, RoomID: homeID,
		PayloadJSON: payloadJSON(map[string]string{"item_id": itemID, "agents": strconv.Itoa(len(affected))})})
	return err
}

// syncPoolChanges runs syncPoolChange for every project that has an open item or an agent a pool could have left behind. A role's own pool
// is global, so its update can touch any project.
func (tx *Tx) syncPoolChanges() error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT project_id FROM agent WHERE pool_origin='pool' AND desired='active' AND revoked_at IS NULL
		UNION SELECT project_id FROM inbox_item WHERE kind='pool_change' AND state='open'`)
	if err != nil {
		return err
	}
	var projects []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range projects {
		if err := tx.syncPoolChange(id); err != nil {
			return err
		}
	}
	return nil
}

// DetectPoolChanges is the server-start pass: it raises the item for any project whose pool already left agents behind (a change made while
// the server was down, or by an older build).
func (s *Store) DetectPoolChanges(ctx context.Context) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.syncPoolChanges() })
}

// poolOriginFor decides the provenance of a new agent. An explicit origin from the caller stands; otherwise an approved deviation is
// "approved" and the rest is read from the pool in force: a choice it lists is "pool", any other is a person's "explicit" choice.
func (tx *Tx) poolOriginFor(projectID, roleKey, origin, approvedBy, launcherID, model string) (string, error) {
	if approvedBy != "" {
		return PoolOriginApproved, nil
	}
	if origin != "" {
		return origin, nil
	}
	pool, _, err := effectivePool(tx.ctx, tx.tx, projectID, roleKey)
	if err != nil {
		return "", err
	}
	if launcherID != "" && poolHas(pool, launcherID, model) {
		return PoolOriginPool, nil
	}
	return PoolOriginExplicit, nil
}

// MarkPoolOrigin records that an agent now runs on a pool choice (a respawn on the pool model); a switch to anything else marks it explicit.
func (s *Store) MarkPoolOrigin(ctx context.Context, agentID, launcherID, model string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE agent SET pool_origin='pool',pool_launcher_id=?,pool_model=?,pool_kept_sig=NULL WHERE id=?`, launcherID, model, agentID)
		return err
	})
}

// poolChangeDecider says the actor may decide a project's pool item: the project's owner, or an admin of the instance who is a member.
func (tx *Tx) poolChangeDecider(projectID, actorID string) error {
	var role string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, actorID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if role == "owner" {
		return nil
	}
	level, err := tx.privilege(actorID)
	if err != nil {
		return err
	}
	if level < privAdmin {
		return ErrForbidden
	}
	return nil
}

// PoolRespawn is one agent queued for a respawn on the pool model.
type PoolRespawn struct {
	AgentID   string
	ProjectID string
	DecidedBy string
}

// DecidePoolChange settles a project's pool_change item. "respawn" queues every listed agent but those named in keep for a switch to the
// pool (the dispatcher pass starts each once no turn of it is running); "keep" leaves them all. Either way the agents kept are recorded
// against the pool in force, so the item does not return until a pool changes again.
func (s *Store) DecidePoolChange(ctx context.Context, itemID, actorID, key, choice string, rev int, keep []string) (RecoveryDecision, bool, error) {
	keep = slices.Clone(keep)
	slices.Sort(keep)
	digest := sha256.Sum256([]byte(itemID + "\x00" + choice + "\x00" + strconv.Itoa(rev) + "\x00" + fmt.Sprint(keep)))
	result, reused, err := s.WithRequestKey(ctx, RequestKey{ActorKind: "user", ActorID: actorID,
		Kind: "recovery_decision", Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *Tx) (string, error) {
		var projectID, state, kind, roomID string
		var currentRev int64
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,state,kind,room_id,rev FROM inbox_item WHERE id=?`, itemID).Scan(&projectID, &state, &kind, &roomID, &currentRev)
		if errors.Is(err, sql.ErrNoRows) || err == nil && kind != "pool_change" {
			return "", ErrForbidden
		}
		if err != nil {
			return "", err
		}
		if err := tx.poolChangeDecider(projectID, actorID); err != nil {
			return "", err
		}
		if choice != PoolChangeRespawn && choice != PoolChangeKeep {
			return "", ErrChoiceNotOffered
		}
		if currentRev != int64(rev) {
			return "", ErrStaleInboxRev
		}
		if state != "open" {
			return "", ErrAlreadyResolved
		}
		affected, err := poolAffected(tx.ctx, tx.tx, projectID)
		if err != nil {
			return "", err
		}
		queued, kept := 0, 0
		sigs := map[string]string{}
		for _, a := range affected {
			sig, ok := sigs[a.RoleKey]
			if !ok {
				if _, sig, err = effectivePool(tx.ctx, tx.tx, projectID, a.RoleKey); err != nil {
					return "", err
				}
				sigs[a.RoleKey] = sig
			}
			if choice == PoolChangeRespawn && !slices.Contains(keep, a.AgentID) {
				if _, err := tx.Exec(`INSERT OR IGNORE INTO pool_respawn(agent_id,project_id,decided_by,requested_at) VALUES (?,?,?,?)`,
					a.AgentID, projectID, actorID, timestamp()); err != nil {
					return "", err
				}
				queued++
				continue
			}
			if _, err := tx.Exec(`UPDATE agent SET pool_kept_sig=? WHERE id=?`, sig, a.AgentID); err != nil {
				return "", err
			}
			kept++
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=?`, actorID, timestamp(), itemID); err != nil {
			return "", err
		}
		if _, err := tx.AppendEvent(Event{Kind: "pool_change_decided", ProjectID: projectID,
			PayloadJSON: payloadJSON(map[string]string{"item_id": itemID, "choice": choice, "actor_id": actorID,
				"respawn": strconv.Itoa(queued), "kept": strconv.Itoa(kept)})}); err != nil {
			return "", err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "choice": choice, "actor": actorID})}); err != nil {
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

// PoolRespawnsReady lists the queued respawns whose agent has no turn running. A queued agent that has stopped, left the pool's reach (it
// runs on a pool choice already) or been switched meanwhile is dropped from the queue here.
func (s *Store) PoolRespawnsReady(ctx context.Context) ([]PoolRespawn, error) {
	var ready []PoolRespawn
	err := s.Tx(ctx, func(tx *Tx) error {
		ready = nil
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT p.agent_id,p.project_id,p.decided_by,a.desired,a.revoked_at IS NOT NULL,a.pool_origin,r.role_key,COALESCE(a.launcher_id,''),a.model,
			EXISTS(SELECT 1 FROM turn t WHERE t.agent_id=a.id AND `+openFlowTurnPredicate+`),
			EXISTS(SELECT 1 FROM agent_switch s WHERE s.agent_id=a.id AND s.state IN ('draining','starting'))
			FROM pool_respawn p JOIN agent a ON a.id=p.agent_id JOIN role r ON r.id=a.role_id ORDER BY p.requested_at,p.agent_id`)
		if err != nil {
			return err
		}
		type queued struct {
			PoolRespawn
			desired, roleKey, launcher, model, origin string
			revoked, busy, switching                  bool
		}
		var all []queued
		for rows.Next() {
			var q queued
			if err := rows.Scan(&q.AgentID, &q.ProjectID, &q.DecidedBy, &q.desired, &q.revoked, &q.origin, &q.roleKey, &q.launcher, &q.model, &q.busy, &q.switching); err != nil {
				rows.Close()
				return err
			}
			all = append(all, q)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, q := range all {
			pool, _, err := effectivePool(tx.ctx, tx.tx, q.ProjectID, q.roleKey)
			if err != nil {
				return err
			}
			// An agent a person switched away from the pool (origin no longer "pool") left the queue's reach with its switch: drop it here too,
			// even when the switch path already deleted the row (an older row, or a switch that began before this pass read the queue).
			if q.revoked || q.desired != "active" || q.origin != PoolOriginPool || poolHas(pool, q.launcher, q.model) {
				if _, err := tx.Exec(`DELETE FROM pool_respawn WHERE agent_id=?`, q.AgentID); err != nil {
					return err
				}
				continue
			}
			if !q.busy && !q.switching {
				ready = append(ready, q.PoolRespawn)
			}
		}
		return nil
	})
	return ready, err
}

// FinishPoolRespawn takes an agent off the respawn queue, whether its switch began or could not.
func (s *Store) FinishPoolRespawn(ctx context.Context, agentID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`DELETE FROM pool_respawn WHERE agent_id=?`, agentID)
		return err
	})
}

// PoolRespawnsQueued says some respawn waits in the queue, so the dispatcher pass pays for a look at it only then.
func (s *Store) PoolRespawnsQueued(ctx context.Context) (bool, error) {
	var queued bool
	err := s.readDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pool_respawn)`).Scan(&queued)
	return queued, err
}

// FailPoolRespawn takes an agent off the respawn queue because its switch could not begin, and says why where the person will see it: the
// project's pool_change item is raised again (or updated, when one is open) listing the agent with "respawn failed: <reason>", so the
// person can try the respawn again or keep the model. The agent itself is untouched.
func (s *Store) FailPoolRespawn(ctx context.Context, agentID, reason string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, agentID).Scan(&projectID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM pool_respawn WHERE agent_id=?`, agentID); err != nil {
			return err
		}
		if err := tx.syncPoolChange(projectID); err != nil {
			return err
		}
		var itemID, options string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,COALESCE(options_json,'') FROM inbox_item WHERE kind='pool_change' AND project_id=? AND state='open'`, projectID).Scan(&itemID, &options)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the agent no longer qualifies (it stopped or is in the pool): nothing to show
		}
		if err != nil {
			return err
		}
		var ask inboxAsk
		if err := json.Unmarshal([]byte(options), &ask); err != nil {
			return err
		}
		if ask.RespawnFailures == nil {
			ask.RespawnFailures = map[string]string{}
		}
		ask.RespawnFailures[agentID] = "respawn failed: " + reason
		encoded, err := json.Marshal(ask)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET options_json=?,rev=rev+1 WHERE id=?`, string(encoded), itemID); err != nil {
			return err
		}
		var roomID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM inbox_item WHERE id=?`, itemID).Scan(&roomID); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "inbox_item_updated", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "kind": "pool_change"})})
		return err
	})
}
