package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// PrepareDispatch picks one eligible turn for an online agent and records the
// normal turn and its note set atomically. A false result means no work is
// currently dispatchable; queued notes stay queued through pauses and stops.
func (s *Store) PrepareDispatch(ctx context.Context, agentID, afterRoom string) (protocol.Delivery, bool, error) {
	var delivery protocol.Delivery
	ready := false
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, machineID, desired, observed, tool, variant, launcherID, model, provider, sessionID, roleID, roleBody, checkoutPath string
		var generation int
		var desiredRev int64
		var blocked, revoked bool
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,a.machine_id,a.desired,a.observed,a.tool,a.variant,COALESCE(a.launcher_id,''),a.model,a.provider,
			a.generation,a.desired_rev,a.recovery_block,a.revoked_at IS NOT NULL,r.id,c.canonical_path
			FROM agent a JOIN role r ON r.id=a.role_id JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, agentID).
			Scan(&projectID, &machineID, &desired, &observed, &tool, &variant, &launcherID, &model, &provider, &generation, &desiredRev, &blocked, &revoked, &roleID, &checkoutPath)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if desired != "active" || revoked {
			return nil
		}
		var clearPending bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent_context_clear WHERE agent_id=?)`, agentID).Scan(&clearPending); err != nil {
			return err
		}
		if clearPending {
			return nil
		}
		// A model the owner has since removed from the harness's list is replaced by its default before this turn starts.
		if model, err = tx.settleAgentModel(agentID, launcherID, model); err != nil {
			return err
		}
		// The instructions are the role's resolved body: a role that extends another carries the parent's text first.
		if roleBody, err = tx.effectiveRoleBody(roleID); err != nil {
			return err
		}
		var flowKey, roleKey string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT p.team_flow_key,r.role_key FROM project p JOIN role r ON r.id=? WHERE p.id=?`, roleID, projectID).Scan(&flowKey, &roleKey); err != nil {
			return err
		}
		if flow, ok := teamFlowTemplate(flowKey); ok {
			if override, ok := flow.RoleTextOverrides[roleKey]; ok {
				roleBody = override
			}
		}
		if flowKey == "leader-dispatcher" && roleKey == "leader" {
			var routingID, routingTitle string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,title FROM room WHERE project_id=? AND team_flow_room_key='routing' AND lifecycle='open' ORDER BY created_at LIMIT 1`, projectID).Scan(&routingID, &routingTitle)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				roleBody += fmt.Sprintf("\n\nHand specifications over in %s (%s).", routingTitle, routingID)
			}
		}
		staffingRole, err := tx.roleLineageContains(roleKey, "leader", "dispatcher")
		if err != nil {
			return err
		}
		if staffingRole {
			if enabled, err := tx.projectReviewerEnabled(projectID); err != nil {
				return err
			} else if !enabled {
				roleBody = withoutReviewerStaffing(roleBody)
			}
		}
		if flowKey == "leader-dispatcher" && (roleKey == "worker" || roleKey == "reviewer" || strings.HasSuffix(roleKey, "-reviewer")) {
			roleBody += "\n\nIn a task room, report to the agent who briefed you (normally the dispatcher) and @mention them; a handoff reaches the dispatcher. Do not @mention the leader there; the leader is not a member of task rooms in this flow."
		}
		if roleBody, err = tx.withInlinePlan(roleID, roleBody); err != nil {
			return err
		}
		var lead bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT cap_is_lead FROM role WHERE id=?`, roleID).Scan(&lead); err != nil {
			return err
		}
		if lead {
			roleBody += "\n\nWhen spawning a role, strongly prefer its configured default agent. Call spawn_agent with role_key and omit tool, variant, and model to select the next available default. If you need a different harness or model, provide a concrete reason. If no default is available, choose a listed harness and model yourself and tell the room why."
			if !strings.Contains(roleBody, "Maintain the project handbook") {
				roleBody += "\n\nMaintain the project handbook with handbook_get and handbook_update at decisions and handoffs. At close-out, update Current state and Open backlog and append a concise Timeline entry. Keep canonical details in repository docs. If the handbook exceeds about 1000 lines, compress its oldest Timeline entries."
			}
		}
		overrides, err := tx.promptOverrides(projectID)
		if err != nil {
			return err
		}
		if s.ToolHold(tool) != "" || s.dispatchHeldForSwitch(tool) {
			// The limit guard judges the host machine's account, so only agents on the host wait for it: a remote machine
			// has its own login and its own quota.
			var onHost bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT is_host=1 FROM machine WHERE id=?`, machineID).Scan(&onHost); err != nil {
				return err
			}
			if onHost {
				return nil
			}
		}
		limited, err := tx.providerLimitBlocked(agentID, tool)
		if err != nil {
			return err
		}
		if limited {
			return nil
		}
		var switchPending bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent_switch WHERE agent_id=? AND state IN ('starting','failed'))`, agentID).Scan(&switchPending); err != nil {
			return err
		}
		if switchPending {
			return nil
		}
		var gate string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE((SELECT gate FROM dispatch_switch_gate WHERE machine_id=? AND tool=?),'open')`, machineID, tool).Scan(&gate); err != nil {
			return err
		}
		if gate != "open" {
			return nil
		}
		var special Turn
		var handoffTurn bool
		var lifecycle, roomGate, projectGate string
		var parentID sql.NullString
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT t.id,t.session_id,t.generation,t.room_id,t.parent_turn_id,t.attempt,t.mode,t.exec,
			r.lifecycle,r.gate,p.gate,EXISTS(SELECT 1 FROM switch_handoff_turn ht WHERE ht.turn_id=t.id) FROM turn t JOIN room r ON r.id=t.room_id JOIN agent ag ON ag.id=t.agent_id JOIN project p ON p.id=ag.project_id
			WHERE t.agent_id=? AND t.exec='pending' AND NOT EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested') AND (t.mode IN ('correction','diagnostic')
			OR EXISTS(SELECT 1 FROM switch_handoff_turn ht WHERE ht.turn_id=t.id)
			OR (t.mode='normal' AND t.parent_turn_id IS NOT NULL AND EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind IN ('recovery_decision','turn_limit_continuation','turn_interrupted_continuation'))))
			ORDER BY t.id LIMIT 1`, agentID).
			Scan(&special.ID, &special.SessionID, &special.Generation, &special.RoomID, &parentID, &special.Attempt, &special.Mode, &special.Exec, &lifecycle, &roomGate, &projectGate, &handoffTurn)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			special.ParentTurnID = parentID.String
			if special.Mode == "normal" && !handoffTurn {
				var unsafeOverlap bool
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.exec='lost' AND NOT EXISTS(
					SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind IN ('recovery_quiescent','recovery_overlap_override'))
					FROM turn t WHERE t.id=?`, special.ParentTurnID).Scan(&unsafeOverlap); err != nil {
					return err
				}
				if unsafeOverlap {
					return nil
				}
			}
			if lifecycle != "open" || roomGate != "open" || projectGate != "open" || special.Generation != generation {
				return nil
			}
			if observed != "idle" && (special.Mode != "correction" || observed != "in_turn") {
				return nil
			}
			if special.Mode == "normal" && (blocked) {
				return nil
			}
			var other int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE agent_id=? AND id<>? AND exec IN ('pending','running')`, agentID, special.ID).Scan(&other); err != nil {
				return err
			}
			if other > 0 {
				return nil
			}
			contextRooms, err := tx.agentRoomSnapshot(agentID)
			if err != nil {
				return err
			}
			contextData := map[string]any{"role_body": roleBody, "rooms": contextRooms,
				"control_changes": map[string]any{"desired": "active", "desired_rev": desiredRev}}
			var handoffRequest string
			if handoffTurn {
				handoffRequest = "This is the dedicated switch handoff turn. Return exactly one message in this room. Its body must be a JSON object with string fields current_task, status, work_state (branches, SHAs, files), next_steps, open_questions, and cautions. Do not send ordinary room updates or perform new work; describe verified state for your successor."
				var departing bool
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM team_flow_migration_agent ma JOIN team_flow_migration m ON m.id=ma.migration_id WHERE ma.agent_id=? AND ma.action='handoff_stop' AND m.state='draining')`, agentID).Scan(&departing); err != nil {
					return err
				}
				if departing {
					handoffRequest = "This team flow is changing and your role is departing. Hand off to the leader: in-flight task rooms, workers you manage, current work and what remains. Return exactly one message in this room. Its body must be a JSON object with string fields current_task, status, work_state, next_steps, open_questions, and cautions. Do not start new work."
				}
				contextData["switch_handoff_request"] = handoffRequest
				started := time.Now().UTC()
				if _, err := tx.Exec(`UPDATE switch_handoff SET status='handing_off',started_at=?,handoff_deadline=? WHERE agent_id=? AND turn_id=? AND status='pending'`, started.Format(timestampLayout), started.Add(switchHandoffWindow).Format(timestampLayout), agentID, special.ID); err != nil {
					return err
				}
			}
			if overrides != nil {
				contextData["prompt_overrides"] = overrides
			}
			specialNotes := []protocol.DeliveryNote{}
			originalBodies := map[string]string{}
			if handoffTurn {
				// The handoff is the task for this turn. Put it in notes as well as
				// context so a harness with no queued room messages still answers.
				id := "handoff:" + special.ID
				specialNotes = append(specialNotes, protocol.DeliveryNote{NoteID: id, MessageID: id, RoomID: special.RoomID,
					From: "system", Body: handoffRequest, Why: "dedicated handoff request"})
				originalBodies[id] = handoffRequest
			}
			var allowedTools []string
			if special.Mode != "normal" {
				allowedTools = []string{}
			}
			if special.Mode == "normal" && !handoffTurn {
				var choice string
				// A limit continuation has no recovery decision behind it: an absent choice is not an error, it only skips the recovery evidence.
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE((SELECT json_extract(payload_json,'$.choice') FROM event WHERE turn_id=? AND kind='recovery_decision' ORDER BY seq DESC LIMIT 1),'')`, special.ID).Scan(&choice); err != nil {
					return err
				}
				rows, err := tx.tx.QueryContext(tx.ctx, `SELECT n.id,m.id,m.no,m.room_id,m.author_id,m.body,COALESCE(m.reply_to,''),COALESCE(m.thread_root,''),COALESCE(m.visual_plan_json,''),COALESCE(m.appendix_pages_json,''),n.wake_reason FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id WHERE tn.turn_id=? AND n.state='queued' ORDER BY m.seq`, special.ID)
				if err != nil {
					return err
				}
				var specialReasons []string
				for rows.Next() {
					var note protocol.DeliveryNote
					var planJSON, pagesJSON, wakeReason string
					if err := rows.Scan(&note.NoteID, &note.MessageID, &note.No, &note.RoomID, &note.From, &note.Body, &note.ReplyTo, &note.ThreadRoot, &planJSON, &pagesJSON, &wakeReason); err != nil {
						rows.Close()
						return err
					}
					originalBodies[note.MessageID] = note.Body
					note.Body = deliveredNoteBody(note.Body, note.No, planJSON, pagesJSON)
					specialNotes = append(specialNotes, note)
					specialReasons = append(specialReasons, wakeReason)
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					return err
				}
				if err := tx.noteAttachments(specialNotes); err != nil {
					return err
				}
				if err := tx.noteContext(agentID, specialNotes, specialReasons); err != nil {
					return err
				}
				if choice == "resume" {
					var sourceExec, sourceOutcome string
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT exec,outcome FROM turn WHERE id=?`, special.ParentTurnID).Scan(&sourceExec, &sourceOutcome); err != nil {
						return err
					}
					var report sql.NullString
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT report_json FROM turn WHERE parent_turn_id=? AND mode='diagnostic' ORDER BY id DESC LIMIT 1`, special.ParentTurnID).Scan(&report); err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					}
					contextData["recovery_evidence"] = map[string]any{"origin_turn_id": special.ParentTurnID,
						"exec": sourceExec, "outcome": sourceOutcome, "diagnostic_report": report.String,
						"instruction": "Continue the work from the evidence; do not repeat completed actions."}
				}
			}
			if special.Mode == "correction" {
				var receiptJSON string
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT result_json FROM receipt WHERE turn_id=? AND answer_rev=0`, special.ParentTurnID).Scan(&receiptJSON); err != nil {
					return err
				}
				var receipt AnswerReceipt
				if err := json.Unmarshal([]byte(receiptJSON), &receipt); err != nil {
					return err
				}
				contextData["correction_error"] = receipt.Error
			}
			roster, self, more, err := tx.roomAgentRoster(special.RoomID, agentID, specialNotes, originalBodies)
			if err != nil {
				return err
			}
			contextData["team_roster"] = roster
			contextData["you_are"] = self
			if more > 0 {
				contextData["team_roster_more"] = fmt.Sprintf("%d more", more)
			}
			// A recovered turn re-delivers notes too: the #N they mention are put into its context the same way (a harness without the
			// message_get tool has no other way to read them).
			var recoveredBodies []string
			recoveredNumbers := map[int64]bool{}
			for _, note := range specialNotes {
				recoveredBodies = append(recoveredBodies, note.Body)
				recoveredNumbers[note.No] = true
			}
			recoveredRefs, err := tx.referencedMessages(agentID, projectID, recoveredBodies, recoveredNumbers)
			if err != nil {
				return err
			}
			if err := tx.recordTurnRoomReads(special.ID, recoveredRefs); err != nil {
				return err
			}
			if len(recoveredRefs) > 0 {
				contextData["referenced_messages"] = recoveredRefs
			}
			if lead {
				handbook, err := tx.handbookForDelivery(projectID)
				if err != nil {
					return err
				}
				var nativeStarted sql.NullString
				var sessionTurns int
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT native_session_started_at FROM session WHERE id=?`, special.SessionID).Scan(&nativeStarted); err != nil {
					return err
				}
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE session_id=?`, special.SessionID).Scan(&sessionTurns); err != nil {
					return err
				}
				if !nativeStarted.Valid || sessionTurns == 1 {
					contextData["project_handbook"] = handbook
					contextData["project_handbook_note"] = "This handbook is state from the previous leader. Verify it against current project and room evidence before relying on it."
				} else if handbook.OverBudget {
					contextData["project_handbook_nudge"] = handbook.Nudge
				}
			}
			contextJSON, err := json.Marshal(contextData)
			if err != nil {
				return err
			}
			handle, started, forking, err := sessionNativeFork(tx, special.SessionID)
			if err != nil {
				return err
			}
			legacyCopy, err := tx.importLegacySession(agentID, special.SessionID)
			if err != nil {
				return err
			}
			launchPath, err := tx.agentLaunchPath(agentID, tool, checkoutPath)
			if err != nil {
				return err
			}
			providers, err := tx.providersForTurn(launcherID, tool, model)
			if err != nil {
				return err
			}
			delivery = protocol.Delivery{AgentID: agentID, Generation: generation, DesiredRev: desiredRev, TurnID: special.ID, Providers: providers,
				Mode: adapter.Mode(special.Mode), ParentTurnID: special.ParentTurnID, Notes: specialNotes,
				Agent: adapter.AgentSpec{ID: agentID, Tool: tool, Variant: variant, LauncherID: launcherID, Model: model, Provider: provider}, Context: contextJSON, AllowedTools: allowedTools,
				NativeSessionHandle: handle, NativeSessionStarted: started, ImportLegacySession: legacyCopy, ForkSession: forking, CheckoutPath: checkoutPath, LaunchPath: launchPath}
			ready = true
			return nil
		}
		var continuityBlocksNormal bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM switch_handoff h JOIN agent_switch sw ON sw.agent_id=h.agent_id WHERE h.agent_id=? AND sw.state='draining' AND h.status IN ('waiting','pending','handing_off','ready','automatic'))
			OR EXISTS(SELECT 1 FROM team_flow_migration_agent ma JOIN team_flow_migration m ON m.id=ma.migration_id WHERE ma.agent_id=? AND m.state='draining')`, agentID, agentID).Scan(&continuityBlocksNormal); err != nil {
			return err
		}
		if continuityBlocksNormal {
			return nil
		}
		if observed != "idle" || blocked {
			return nil
		}
		var openTurns int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn t WHERE t.agent_id=? AND (t.exec IN ('pending','running') OR t.settle='pending' OR (t.exec='lost' AND NOT EXISTS(SELECT 1 FROM event e WHERE e.kind='recovery_override' AND (e.turn_id=t.id OR e.turn_id=t.parent_turn_id))))`, agentID).Scan(&openTurns); err != nil {
			return err
		}
		if openTurns > 0 {
			return nil
		}
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM session WHERE agent_id=? AND generation=? AND ended_at IS NULL ORDER BY started_at DESC LIMIT 1`, agentID, generation).Scan(&sessionID); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT n.room_id,MIN(n.created_at) FROM note n
			JOIN room rm ON rm.id=n.room_id JOIN project p ON p.id=?
			JOIN room_member mb ON mb.room_id=rm.id AND mb.member_kind='agent' AND mb.member_id=n.agent_id AND mb.left_at IS NULL
			WHERE n.agent_id=? AND n.state='queued' AND rm.lifecycle='open' AND rm.gate='open' AND p.gate='open'
			AND rm.project_id IN (?,?) GROUP BY n.room_id ORDER BY MIN(n.created_at),n.room_id`, projectID, agentID, projectID, AgentDMProjectID)
		if err != nil {
			return err
		}
		var rooms []string
		for rows.Next() {
			var room, oldest string
			if err := rows.Scan(&room, &oldest); err != nil {
				rows.Close()
				return err
			}
			rooms = append(rooms, room)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(rooms) == 0 {
			return nil
		}
		roomID := rooms[0]
		if len(rooms) > 1 && afterRoom != "" {
			for i, room := range rooms {
				if room == afterRoom {
					roomID = rooms[(i+1)%len(rooms)]
					break
				}
			}
		}
		// Message expiry keeps old ambient chatter out of an agent's context. A note that
		// addresses the agent (a mention, a role mention, a reply, a DM, a handoff, an
		// inbox answer) is always delivered, however old; a room-wide note (a group tag, a
		// human message addressed to nobody in particular) older than the window is dropped
		// and the turn says so in an `expired` marker. Nothing is deleted: the message stays,
		// and recall still finds it.
		expiryWindow, err := tx.messageExpirySeconds()
		if err != nil {
			return err
		}
		expiryCutoff := ""
		if expiryWindow > 0 {
			expiryCutoff = time.Now().UTC().Add(-time.Duration(expiryWindow) * time.Second).Format(timestampLayout)
		}
		rows, err = tx.tx.QueryContext(tx.ctx, `SELECT n.id,m.id,m.no,m.room_id,m.author_id,m.body,COALESCE(m.reply_to,''),COALESCE(m.thread_root,''),COALESCE(m.visual_plan_json,''),COALESCE(m.appendix_pages_json,''),m.author_kind,
			EXISTS(SELECT 1 FROM provider_limit_retry retry WHERE retry.note_id=n.id AND retry.caution=1),n.wake_reason,m.created_at,
			COALESCE((SELECT r.kind FROM room r WHERE r.id=m.room_id),'')
			FROM note n JOIN message m ON m.id=n.message_id WHERE n.agent_id=? AND n.room_id=? AND n.state='queued'
			ORDER BY m.seq LIMIT 20`, agentID, roomID)
		if err != nil {
			return err
		}
		var notes []protocol.DeliveryNote
		var reasons []string
		var expiredNotes []expiredNote
		originalBodies := map[string]string{}
		agentTriggered, humanTriggered := false, false
		for rows.Next() {
			var note protocol.DeliveryNote
			var authorKind string
			var planJSON, pagesJSON string
			var retryCaution bool
			var wakeReason, createdAt, roomKind string
			if err := rows.Scan(&note.NoteID, &note.MessageID, &note.No, &note.RoomID, &note.From, &note.Body, &note.ReplyTo, &note.ThreadRoot, &planJSON, &pagesJSON, &authorKind, &retryCaution, &wakeReason, &createdAt, &roomKind); err != nil {
				rows.Close()
				return err
			}
			if expiryCutoff != "" && expirableNote(wakeReason, authorKind, note.ReplyTo, roomKind) && createdAt <= expiryCutoff {
				expiredNotes = append(expiredNotes, expiredNote{noteID: note.NoteID, messageID: note.MessageID})
				continue
			}
			originalBodies[note.MessageID] = note.Body
			note.Body = deliveredNoteBody(note.Body, note.No, planJSON, pagesJSON)
			if retryCaution {
				note.RetryNotice = "The previous attempt was interrupted before it finished (for example by a provider limit or a lost session); you may have already done part of it. Check the state before acting."
			}
			if authorKind == "agent" {
				agentTriggered = true
			}
			if authorKind == "user" {
				humanTriggered = true
			}
			notes = append(notes, note)
			reasons = append(reasons, wakeReason)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if err := tx.noteAttachments(notes); err != nil {
			return err
		}
		if err := tx.noteContext(agentID, notes, reasons); err != nil {
			return err
		}
		if len(notes) == 0 {
			// Nothing left to read: the expired notes are dropped so they do not wake the agent again.
			_, err := tx.dropExpiredNotes(projectID, roomID, agentID, expiredNotes, expiryCutoff)
			return err
		}
		if agentTriggered && !humanTriggered {
			var lastExit sql.NullString
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT MAX(ended_at) FROM turn WHERE agent_id=? AND room_id=? AND mode='normal'`, agentID, roomID).Scan(&lastExit); err != nil {
				return err
			}
			if lastExit.Valid && lastExit.String > time.Now().UTC().Add(-2*time.Second).Format("2006-01-02T15:04:05.000000000Z") {
				return nil
			}
		}
		window := time.Now().UTC().Add(-10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z")
		var turnsInWindow int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM event e JOIN turn t ON t.id=e.turn_id
			WHERE e.kind='turn_recorded' AND e.agent_id=? AND e.room_id=? AND e.created_at>=? AND t.mode='normal'
			AND e.seq>COALESCE((SELECT MAX(c.seq) FROM event c WHERE c.agent_id=? AND c.kind='agent_control_requested' AND json_extract(c.payload_json,'$.desired')='active'),0)
			AND (t.exec IN ('running','exited','lost')
			  OR EXISTS(SELECT 1 FROM event started WHERE started.turn_id=t.id AND started.kind='turn_ack_accepted')
			  OR (EXISTS(SELECT 1 FROM event admitted WHERE admitted.turn_id=t.id AND admitted.kind='turn_dispatch_requested')
			      AND NOT EXISTS(SELECT 1 FROM event unsent WHERE unsent.turn_id=t.id AND unsent.kind IN ('turn_never_started','turn_dispatch_cancelled'))))`, agentID, roomID, window, agentID).Scan(&turnsInWindow); err != nil {
			return err
		}
		if turnsInWindow >= 20 {
			if _, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=? AND desired='active'`, agentID); err != nil {
				return err
			}
			var openRecovery int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM inbox_item WHERE agent_id=? AND kind='recovery' AND state='open'`, agentID).Scan(&openRecovery); err != nil {
				return err
			}
			if openRecovery > 0 {
				return nil
			}
			id, err := newID("inb")
			if err != nil {
				return err
			}
			options := `{"text":"Agent paused after 20 turns in 10 minutes to prevent an automatic reply loop.","options":["Continue","Abandon"]}`
			if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at) VALUES (?,?,?,?,'agent',?,'recovery',?,?)`, id, projectID, roomID, agentID, agentID, options, timestamp()); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: roomID, AgentID: agentID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "recovery"})}); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "agent_turn_budget_exceeded", ProjectID: projectID, RoomID: roomID, AgentID: agentID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id})}); err != nil {
				return err
			}
			if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: "Agent paused after 20 turns in 10 minutes; review its queued work."}); err != nil {
				return err
			}
			return nil
		}
		// A turn is going to be built, so the expired notes are dropped now, with the marker that tells the agent.
		// Dropping them earlier would lose the marker when the cooldown or the turn cap ends this call.
		expiredMarker, err := tx.dropExpiredNotes(projectID, roomID, agentID, expiredNotes, expiryCutoff)
		if err != nil {
			return err
		}
		turn, err := tx.RecordTurn(Turn{AgentID: agentID, SessionID: sessionID, Generation: generation, RoomID: roomID, Attempt: 1, Mode: "normal", Exec: "pending"})
		if err != nil {
			return err
		}
		for _, note := range notes {
			if _, err := tx.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note.NoteID); err != nil {
				return err
			}
		}
		knowledge, err := tx.knowledgeSnapshot(projectID, roomID)
		if err != nil {
			return err
		}
		if shouldTrimKnowledgeForAgent(tool, model) {
			knowledge = trimKnowledgeSnapshotForSlowHarness(knowledge)
		}
		roster, self, more, err := tx.roomAgentRoster(roomID, agentID, notes, originalBodies)
		if err != nil {
			return err
		}
		contextRooms, err := tx.agentRoomSnapshot(agentID)
		if err != nil {
			return err
		}
		// The role body and the knowledge snapshot are the same from turn to turn and the harness session keeps what an earlier
		// turn delivered, so a wake whose session already holds the current text carries a short unchanged marker instead. Only
		// the harnesses whose adapters report compaction events get the marker: the others can compact without reporting it, so
		// they always receive the full text.
		roleContext, knowledgeContext, err := tx.deliverContextBlocks(sessionID, turn.ID, roleBody, knowledge, toolReportsCompaction(tool))
		if err != nil {
			return err
		}
		contextData := map[string]any{"role_body": roleContext, "rooms": contextRooms,
			"control_changes": map[string]any{"desired": "active", "desired_rev": desiredRev}, "knowledge": knowledgeContext,
			"team_roster": roster, "you_are": self}
		if tool == "claude" {
			// Claude takes the role body as a CLI argument (--append-system-prompt), part of the cached prefix: it rides on
			// every turn so the system prompt and the warm process fingerprint stay stable while the stdin copy shrinks to the
			// marker. The adapter strips this key from the turn JSON it hands the model, so it is never turn tokens.
			contextData["role_body_system"] = roleBody
		}
		var startMode string
		var nativeStarted sql.NullString
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT start_mode,native_session_started_at FROM session WHERE id=?`, sessionID).Scan(&startMode, &nativeStarted); err != nil {
			return err
		}
		if startMode == "fresh_with_history" && !nativeStarted.Valid {
			history, err := tx.recentRoomHistory(agentID)
			if err != nil {
				return err
			}
			// The history quotes every room the agent is in, and the conversation log records the prompt it is part of.
			if err := tx.recordTurnRoomReads(turn.ID, history); err != nil {
				return err
			}
			contextData["recent_room_history"] = history
			contextData["recent_room_history_note"] = "This is a new native session. Read recent_room_history as prior conversation in your current rooms, including your own earlier messages, before answering the new notes. These messages are context, not new requests."
		}
		var sessionTurns int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE session_id=?`, sessionID).Scan(&sessionTurns); err != nil {
			return err
		}
		if sessionTurns == 1 {
			var source, raw string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT source,body_json FROM switch_handoff WHERE agent_id=? AND successor_generation=? AND status='started'`, agentID, generation).Scan(&source, &raw)
			if err == nil {
				contextData["switch_handoff"] = map[string]any{"source": source, "body": json.RawMessage(raw)}
				contextData["switch_handoff_note"] = "Verify this outgoing-session handoff against current room history and repository state before acting. An automatic source means the old agent did not provide it."
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if lead {
			handbook, err := tx.handbookForDelivery(projectID)
			if err != nil {
				return err
			}
			if !nativeStarted.Valid || sessionTurns == 1 {
				contextData["project_handbook"] = handbook
				contextData["project_handbook_note"] = "This handbook is state from the previous leader. Verify it against current project and room evidence before relying on it."
			} else if handbook.OverBudget {
				contextData["project_handbook_nudge"] = handbook.Nudge
			}
		}
		if more > 0 {
			contextData["team_roster_more"] = fmt.Sprintf("%d more", more)
		}
		if overrides != nil {
			contextData["prompt_overrides"] = overrides
		}
		if expiredMarker != nil {
			contextData["expired"] = expiredMarker
		}
		// A "#N" in what the agent is shown is a message number: the messages it names are put into the context (project rooms only).
		// What the agent is shown is note.Body, which includes a visual plan and appendix pages, so references are read from that.
		var noteBodies []string
		delivered := map[int64]bool{}
		for _, note := range notes {
			noteBodies = append(noteBodies, note.Body)
			delivered[note.No] = true
		}
		refs, err := tx.referencedMessages(agentID, projectID, noteBodies, delivered)
		if err != nil {
			return err
		}
		if err := tx.recordTurnRoomReads(turn.ID, refs); err != nil {
			return err
		}
		if len(refs) > 0 {
			contextData["referenced_messages"] = refs
		}
		contextJSON, err := json.Marshal(contextData)
		if err != nil {
			return err
		}
		handle, started, forking, err := sessionNativeFork(tx, sessionID)
		if err != nil {
			return err
		}
		legacyCopy, err := tx.importLegacySession(agentID, sessionID)
		if err != nil {
			return err
		}
		launchPath, err := tx.agentLaunchPath(agentID, tool, checkoutPath)
		if err != nil {
			return err
		}
		providers, err := tx.providersForTurn(launcherID, tool, model)
		if err != nil {
			return err
		}
		delivery = protocol.Delivery{AgentID: agentID, Generation: generation, DesiredRev: desiredRev, TurnID: turn.ID, Mode: adapter.ModeNormal, Notes: notes, Providers: providers,
			Agent: adapter.AgentSpec{ID: agentID, Tool: tool, Variant: variant, LauncherID: launcherID, Model: model, Provider: provider}, Context: contextJSON,
			NativeSessionHandle: handle, NativeSessionStarted: started, ImportLegacySession: legacyCopy, ForkSession: forking, CheckoutPath: checkoutPath, LaunchPath: launchPath}
		ready = true
		return nil
	})
	return delivery, ready, err
}

// agentRoomSnapshot names every open room membership, including restricted
// rooms. The snapshot is information for the agent and grants no new access.
func (tx *Tx) agentRoomSnapshot(agentID string) ([]map[string]string, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT r.id,r.title,r.kind FROM room_member m JOIN room r ON r.id=m.room_id
		WHERE m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL AND r.lifecycle='open'
		ORDER BY r.created_at,r.id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rooms := []map[string]string{}
	for rows.Next() {
		var id, title, kind string
		if err := rows.Scan(&id, &title, &kind); err != nil {
			return nil, err
		}
		rooms = append(rooms, map[string]string{"room_id": id, "title": title, "kind": kind})
	}
	return rooms, rows.Err()
}

func (tx *Tx) importLegacySession(agentID, sessionID string) (bool, error) {
	var needed bool
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.resumed_from_legacy_id IS NOT NULL AND a.tool IN ('codex','pi','opencode','vibe')
		AND s.start_mode='native_resume' AND s.native_session_started_at IS NOT NULL
		AND NOT EXISTS(SELECT 1 FROM turn t JOIN session completed ON completed.id=t.session_id
			WHERE t.agent_id=a.id AND completed.start_mode='native_resume' AND completed.native_session_id=s.native_session_id
			AND t.exec='exited' AND t.outcome='completed' AND t.settle='settled')
		FROM agent a JOIN session s ON s.agent_id=a.id WHERE a.id=? AND s.id=?`, agentID, sessionID).Scan(&needed)
	return needed, err
}

// recentRoomHistory gives a new native session bounded conversation context
// from every room the agent currently belongs to. It reads messages directly,
// including imported history and the agent's own replies, rather than only
// queued notes (which omit messages seen by the previous native session).
func (tx *Tx) recentRoomHistory(agentID string) ([]AgentMessage, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT m.no,r.id,r.title,m.author_kind,m.author_id,m.body,m.created_at
		FROM message m JOIN room r ON r.id=m.room_id
		JOIN room_member rm ON rm.room_id=r.id AND rm.member_kind='agent' AND rm.member_id=? AND rm.left_at IS NULL
		WHERE m.no>0 ORDER BY m.seq DESC LIMIT 40`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history := make([]AgentMessage, 0, 40)
	type author struct{ kind, id string }
	authors := make([]author, 0, 40)
	for rows.Next() {
		var item AgentMessage
		var kind, authorID, body string
		if err := rows.Scan(&item.No, &item.RoomID, &item.Room, &kind, &authorID, &body, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Available = true
		item.Body = excerptRunes(body, 1500)
		history = append(history, item)
		authors = append(authors, author{kind, authorID})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range history {
		history[i].From = authorLabel(tx.ctx, tx.tx, authors[i].kind, authors[i].id)
	}
	for left, right := 0, len(history)-1; left < right; left, right = left+1, right-1 {
		history[left], history[right] = history[right], history[left]
	}
	return history, nil
}

const maxDispatchRoster = 20

// roomAgentRoster snapshots current room membership at dispatch time. Keep the
// recipient, leads, and agents involved in delivered notes ahead of other
// members so large rooms do not expand every prompt without bound.
func (tx *Tx) roomAgentRoster(roomID, agentID string, notes []protocol.DeliveryNote, originalBodies map[string]string) ([]map[string]string, map[string]string, int, error) {
	authors := map[string]bool{}
	mentioned := map[string]string{}
	if len(notes) > 0 {
		recipients, err := tx.roomRecipients(roomID)
		if err != nil {
			return nil, nil, 0, err
		}
		var projectID, projectName string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT p.id,p.name FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`, roomID).Scan(&projectID, &projectName); err != nil {
			return nil, nil, 0, err
		}
		for _, note := range notes {
			authors[note.From] = true
			// Delivery bodies may include rendered visual appendices. Addressing
			// uses the original message body, so rank from that same text.
			body, ok := originalBodies[note.MessageID]
			if !ok {
				return nil, nil, 0, fmt.Errorf("missing original body for message %s", note.MessageID)
			}
			for id, reason := range resolveMentionTargets(body, note.From, recipients, projectID, projectName) {
				if mentionReasonStrength(reason) > mentionReasonStrength(mentioned[id]) {
					mentioned[id] = reason
				}
			}
		}
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT a.id,a.slug,r.role_key,a.tool,a.model,r.cap_is_lead
		FROM room_member mb JOIN agent a ON a.id=mb.member_id JOIN role r ON r.id=a.role_id
		WHERE mb.room_id=? AND mb.member_kind='agent' AND mb.left_at IS NULL AND a.revoked_at IS NULL
		ORDER BY a.slug,a.id`, roomID)
	if err != nil {
		return nil, nil, 0, err
	}
	type member struct {
		data     map[string]string
		priority int
	}
	var members []member
	var self map[string]string
	for rows.Next() {
		var id, name, role, harness, model string
		var lead bool
		if err := rows.Scan(&id, &name, &role, &harness, &model, &lead); err != nil {
			rows.Close()
			return nil, nil, 0, err
		}
		entry := map[string]string{"id": id, "name": name, "role": role, "harness": harness, "model": model}
		priority := 4
		if id == agentID {
			priority = 0
		} else if lead {
			priority = 1
		} else if authors[id] || mentioned[id] == "mention" {
			priority = 2
		} else if mentioned[id] == "role" {
			priority = 3
		}
		members = append(members, member{data: entry, priority: priority})
		if id == agentID {
			self = entry
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, 0, err
	}
	sort.SliceStable(members, func(i, j int) bool { return members[i].priority < members[j].priority })
	more := 0
	if len(members) > maxDispatchRoster {
		more = len(members) - maxDispatchRoster
		members = members[:maxDispatchRoster]
	}
	roster := make([]map[string]string, 0, len(members))
	for _, item := range members {
		roster = append(roster, item.data)
	}
	if self == nil {
		var id, name, role, harness, model string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.id,a.slug,r.role_key,a.tool,a.model
			FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, agentID).
			Scan(&id, &name, &role, &harness, &model); err != nil {
			return nil, nil, 0, err
		}
		self = map[string]string{"id": id, "name": name, "role": role, "harness": harness, "model": model}
	}
	return roster, self, more, nil
}

func (tx *Tx) noteAttachments(notes []protocol.DeliveryNote) error {
	for i := range notes {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,filename,mime,size,sha256 FROM attachment WHERE message_id=? AND removed_at IS NULL ORDER BY created_at,id`, notes[i].MessageID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a protocol.DeliveryAttachment
			if err := rows.Scan(&a.ID, &a.Name, &a.MIME, &a.Size, &a.SHA256); err != nil {
				rows.Close()
				return err
			}
			notes[i].Attachments = append(notes[i].Attachments, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// QueuedAgentIDs gives the server a stable pass over agents. The store still
// applies all mutable eligibility checks in PrepareDispatch and AdmitTurn.
func (s *Store) QueuedAgentIDs(ctx context.Context) ([]string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id FROM note WHERE state='queued'
		UNION SELECT t.agent_id FROM turn t WHERE t.exec='pending' AND (t.mode IN ('correction','diagnostic') OR t.parent_turn_id IS NOT NULL OR EXISTS(SELECT 1 FROM switch_handoff_turn ht WHERE ht.turn_id=t.id))
		AND NOT EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested') ORDER BY agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) TurnDispatchRequested(ctx context.Context, turnID string) (bool, error) {
	var admitted bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='turn_dispatch_requested')`, turnID).Scan(&admitted)
	return admitted, err
}

// CancelPreparedDispatch records a never-sent turn as cancelled. Its notes
// stay queued for the next pass, and the turn remains as audit evidence.
func (s *Store) CancelPreparedDispatch(ctx context.Context, turnID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var exec, agentID, roomID, projectID string
		var recoveryContinuation, admitted bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.exec,t.agent_id,t.room_id,a.project_id,
			EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='recovery_decision'),
			EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested')
			FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, turnID).Scan(&exec, &agentID, &roomID, &projectID, &recoveryContinuation, &admitted); err != nil {
			return err
		}
		if exec != "pending" || recoveryContinuation || admitted {
			return ErrInvalidTransition
		}
		if _, err := tx.Exec(`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=? WHERE id=?`, timestamp(), turnID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "turn_dispatch_cancelled", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID,
			PayloadJSON: payloadJSON(map[string]string{"reason": "not_sent"})})
		return err
	})
}

// SweepUndispatchedNormalTurns clears turns created before a server crash but
// never admitted to a machine. Their notes were never advanced from queued.
func (s *Store) SweepUndispatchedNormalTurns(ctx context.Context) (int, error) {
	count := 0
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT t.id,t.agent_id,t.room_id,a.project_id FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.exec='pending' AND t.mode='normal'
			AND NOT EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind IN ('turn_dispatch_requested','recovery_decision'))`)
		if err != nil {
			return err
		}
		type item struct{ id, agentID, roomID, projectID string }
		items := []item{}
		for rows.Next() {
			var x item
			if err := rows.Scan(&x.id, &x.agentID, &x.roomID, &x.projectID); err != nil {
				rows.Close()
				return err
			}
			items = append(items, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, x := range items {
			if _, err := tx.Exec(`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=? WHERE id=?`, timestamp(), x.id); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "turn_dispatch_cancelled", ProjectID: x.projectID, RoomID: x.roomID, AgentID: x.agentID, TurnID: x.id, PayloadJSON: payloadJSON(map[string]string{"reason": "startup_unsent"})}); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

// expiredNote is a queued ambient note that outlived the expiry window.
type expiredNote struct{ noteID, messageID string }

// ambientWakeReason says whether a note woke the agent by being room-wide rather
// than aimed at it: a group tag (@channel, @here, @ais, ...) or an agent's message that names nobody (a broadcast). Only such a note written
// by an agent is ever expired (AI room chatter); a human's message is always
// delivered, including a human's own group tag. Everything else is never expired
// either: a mention, a role mention, a reply, a DM, a handoff, an inbox answer, and an
// untargeted human message (humans are default-respond for AIs, so a room-wide
// human message is a request, not chatter).
func ambientWakeReason(reason string) bool { return reason == "group" || reason == "broadcast" }

// expirableNote is the whole rule for what message expiry may drop: AI group chatter. A note is expirable only
// when an agent wrote it, it reached this agent through a group tag, it is not a reply (a reply names the
// agent whatever else it carries, such as @ais), and the room is not a private chat (a DM's AI messages are
// addressed to the agent even when they carry a group tag).
func expirableNote(wakeReason, authorKind, replyTo, roomKind string) bool {
	return ambientWakeReason(wakeReason) && authorKind == "agent" && replyTo == "" && roomKind != "dm"
}

// messageExpirySeconds reads the expiry window inside the dispatch transaction.
func (tx *Tx) messageExpirySeconds() (int, error) {
	var value string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT value FROM global_setting WHERE key=?`, MessageExpiryKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultMessageExpirySeconds, nil
	}
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.Atoi(value)
	if convErr != nil || !ValidMessageExpirySeconds(n) {
		return DefaultMessageExpirySeconds, nil
	}
	return n, nil
}

// dropExpiredNotes marks the expired ambient notes dropped (the state a note takes when
// it will not be delivered; the message itself is untouched), records one notes_dropped
// event, and returns the marker the turn carries so the agent knows older room-wide
// messages exist and how to reach them. It returns nil when nothing expired.
func (tx *Tx) dropExpiredNotes(projectID, roomID, agentID string, expired []expiredNote, cutoff string) (map[string]any, error) {
	if len(expired) == 0 {
		return nil, nil
	}
	for _, note := range expired {
		if _, err := tx.Exec(`UPDATE note SET state='dropped' WHERE id=? AND state='queued'`, note.noteID); err != nil {
			return nil, err
		}
	}
	if _, err := tx.AppendEvent(Event{Kind: "notes_dropped", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
		PayloadJSON: payloadJSON(map[string]string{"reason": "expired", "count": fmt.Sprint(len(expired))})}); err != nil {
		return nil, err
	}
	return map[string]any{
		"hidden_count":      len(expired),
		"oldest_id":         expired[0].messageID,
		"newest_expired_id": expired[len(expired)-1].messageID,
		"expired_before":    cutoff,
		"hint":              "Older room-wide AI messages were not delivered but are still stored. Search for them with the recall tool.",
	}, nil
}

// GroupOnlyMessages returns the ids, among the given messages, that dispatch queued at least one note for and whose
// every note has a room-wide wake reason (group, or broadcast). It reads what dispatch actually did, so a handoff, an answer sent to named
// agents, a mention or a role mention (each a direct wake reason) takes a message out, with no second rule that
// could drift from the first. One query per call; callers pass a page of ids.
func (s *Store) GroupOnlyMessages(ctx context.Context, ids []string) (map[string]bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	groupOnly := map[string]bool{}
	for start := 0; start < len(ids); start += 400 {
		end := min(start+400, len(ids))
		marks := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT message_id FROM note WHERE message_id IN (`+marks+`)
			GROUP BY message_id HAVING COUNT(*)>0 AND SUM(wake_reason NOT IN ('group','broadcast'))=0`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			groupOnly[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return groupOnly, nil
}

// ExpirableMessage says whether message expiry can drop a message for the agents it reached: AI group chatter.
// groupOnly is GroupOnlyMessages' answer for the message (every note it caused has the group wake reason); the
// rest is the dispatch rule for what is never dropped: a human's message, a reply and anything in a private chat.
func ExpirableMessage(authorKind, replyTo, roomKind string, groupOnly bool) bool {
	return groupOnly && authorKind == "agent" && replyTo == "" && roomKind != "dm"
}
