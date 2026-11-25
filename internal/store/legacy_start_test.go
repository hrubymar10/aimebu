package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

func TestHarnessNamesMapToTheToolsThatCanResume(t *testing.T) {
	t.Parallel()
	for harness, tool := range map[string]string{"claude-code": "claude", "Claude": "claude", "codex": "codex", "pi": "pi", "opencode": "opencode", "vibe": "vibe", "cursor": "", "aider": "", "": ""} {
		if got := ResumableTool(harness); got != tool {
			t.Fatalf("%q -> %q, want %q", harness, got, tool)
		}
	}
	for id, slug := range map[string]string{"sage@aimebu": "sage", "Ben@aimebu": "ben", "nina": "nina"} {
		if got := LegacySlug(id); got != slug {
			t.Fatalf("%q -> %q", id, got)
		}
	}
	for slug, ok := range map[string]bool{"sage": true, "ab": false, "a_b-c9": true, "9lives": false, "has space": false, "toolongnamethatexceedsthelimit": false} {
		if ValidAgentSlug(slug) != ok {
			t.Fatalf("ValidAgentSlug(%q) != %v", slug, ok)
		}
	}
}

func legacyStartFixture(t *testing.T) (*Store, Project, Room) {
	t.Helper()
	s, project, room, _, _ := wakeFixture(t)
	for _, statement := range []string{
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_start','owner','hash','{}','now')`,
		`INSERT INTO legacy_agent_session(agent_id,harness,model,project,cwd,harness_session_id,registered_at,last_seen) VALUES
			('sage@old','claude-code','opus','old','/tmp/wake/sub','0f7c6a1e-1111-4222-8333-444455556666','2026-10-01T10:00:00Z','2026-10-02T10:00:00Z'),
			('vib@old','vibe','m','old','/tmp/wake','','2026-10-01T10:00:00Z','2026-10-02T10:00:00Z'),
			('far@old','claude-code','m','old','/elsewhere/dir','0f7c6a1e-9999-4222-8333-444455556666','2026-10-01T10:00:00Z','2026-10-02T10:00:00Z')`,
		`INSERT INTO legacy_agent_role(agent_id,v1_room_id,role_key) VALUES ('sage@old','r1','worker'),('sage@old','r2','leader'),('sage@old','r3','leader')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_start','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	// Each legacy agent wrote in the room, which is what lets it be started from there.
	for i, id := range []string{"sage@old", "vib@old", "far@old"} {
		if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES (?,?,?,'agent',?,'from v1','[]','2026-10-02T09:00:00.000000000Z')`, "msg_legacy_"+id, 900000+i, room.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	return s, project, room
}

func startFromLegacy(t *testing.T, s *Store, room Room, in SpawnInput) (AgentCard, error) {
	t.Helper()
	in.RoomID, in.ActorID, in.ActorKind, in.MachineID, in.RoleKey, in.KeyHash = room.ID, "usr_start", "user", "mch_wake", "worker", "hash"
	if in.Tool == "" {
		in.Tool, in.Variant, in.Model = "claude", "native", "opus"
	}
	var card AgentCard
	err := s.Tx(context.Background(), func(tx *Tx) error {
		var err error
		card, err = tx.SpawnAgent(in)
		return err
	})
	return card, err
}

func TestStartFromLegacyContinuesTheSessionKeepsTheNameAndLinksBothWays(t *testing.T) {
	t.Parallel()
	s, _, room := legacyStartFixture(t)
	ctx := context.Background()
	info, err := s.LegacyStart(ctx, room.ProjectID, "sage@old")
	if err != nil || info.Tool != "claude" || info.RoleKey != "leader" || info.Slug != "sage" || info.StartedID != "" {
		t.Fatalf("before: %+v %v", info, err)
	}
	card, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake/sub", Slug: "sage", ResumeLegacyID: "sage@old", ResumeHandle: info.SessionID})
	if err != nil || card.Slug != "sage" {
		t.Fatalf("start: %+v %v", card, err)
	}
	// The session is already started: the first turn resumes it.
	var mode, handle string
	var startedAt *string
	if err := s.db.QueryRow(`SELECT start_mode,native_session_id,native_session_started_at FROM session WHERE agent_id=?`, card.ID).Scan(&mode, &handle, &startedAt); err != nil ||
		mode != "native_resume" || handle != info.SessionID || startedAt == nil || *startedAt == "" {
		t.Fatalf("session: mode=%q handle=%q started=%v err=%v", mode, handle, startedAt, err)
	}
	var started bool
	if err := s.Tx(ctx, func(tx *Tx) error {
		var sessionID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM session WHERE agent_id=?`, card.ID).Scan(&sessionID); err != nil {
			return err
		}
		h, st, err := sessionNativeState(tx, sessionID)
		started = st && h == info.SessionID
		return err
	}); err != nil || !started {
		t.Fatalf("dispatch would not resume: started=%v err=%v", started, err)
	}
	// Both directions are linked.
	again, err := s.LegacyStart(ctx, room.ProjectID, "sage@old")
	if err != nil || again.StartedID != card.ID || again.StartedSlg != "sage" {
		t.Fatalf("legacy link: %+v %v", again, err)
	}
	if again.HistoryRoom.ID != room.ID {
		t.Fatalf("history room after claim: %+v", again.HistoryRoom)
	}
	if old, err := s.LegacyAgentProfile(ctx, room.ID, "sage@old"); err != nil || !old.Imported || old.AgentID != "sage@old" {
		t.Fatalf("historical profile after claim: %+v %v", old, err)
	}
	if from, err := s.LegacyStarted(ctx, card.ID); err != nil || from != "sage@old" {
		t.Fatalf("agent link: %q %v", from, err)
	}
	profile, err := s.AgentProfile(ctx, room.ID, card.ID)
	if err != nil || profile.ResumedFrom != "sage@old" {
		t.Fatalf("profile: %+v %v", profile, err)
	}
	// It cannot be started a second time; that is what is said, not that the name is taken (the first start's agent holds it).
	if _, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake/sub", Slug: "sage", ResumeLegacyID: "sage@old", ResumeHandle: info.SessionID}); !errors.Is(err, ErrLegacyStarted) {
		t.Fatalf("a second start: %v", err)
	}
}

func TestStartFromLegacyClaimsOnlyMessagesInTheStartingProject(t *testing.T) {
	t.Parallel()
	s, project, room := legacyStartFixture(t)
	ctx := context.Background()
	foreign, err := s.CreateProject(ctx, Project{Name: "foreign-claim", Folder: "/tmp/foreign-claim"})
	if err != nil {
		t.Fatal(err)
	}
	foreignRoom, err := s.CreateRoom(ctx, Room{ProjectID: foreign.ID, Kind: "task", Title: "foreign", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	projectDM, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "dm", DMKey: "claim:project", Title: "project dm", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	globalDM, err := s.CreateRoom(ctx, Room{ProjectID: AgentDMProjectID, Kind: "dm", DMKey: "claim:global", Title: "global dm", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{foreignRoom.ID, projectDM.ID, globalDM.ID} {
		if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES (?,?,?,'agent','sage@old','from v1','[]','2026-10-02T09:00:00.000000000Z')`, "msg_claim_"+id, 901000+i, id); err != nil {
			t.Fatal(err)
		}
	}
	card, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake/sub", Slug: "sage", ResumeLegacyID: "sage@old"})
	if err != nil {
		t.Fatal(err)
	}
	newOnly, err := s.CreateProject(ctx, Project{Name: "new-only-claim", Folder: "/tmp/new-only-claim"})
	if err != nil {
		t.Fatal(err)
	}
	newOnlyRoom, err := s.CreateRoom(ctx, Room{ProjectID: newOnly.ID, Kind: "task", Title: "new only", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_claim_v2_only',902000,?,'agent',?,'v2 only','[]','2026-10-03T09:00:00.000000000Z')`, newOnlyRoom.ID, card.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LegacyStart(ctx, newOnly.ID, "sage@old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("v2 activity in another project does not expose v1 identity: %v", err)
	}
	if _, err := s.LegacyAgentProfile(ctx, newOnlyRoom.ID, "sage@old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("v2 activity in another project does not expose a v1 profile: %v", err)
	}
	for _, tc := range []struct{ roomID, want string }{
		{room.ID, card.ID}, {projectDM.ID, card.ID}, {foreignRoom.ID, "sage@old"}, {globalDM.ID, "sage@old"},
	} {
		var got string
		if err := s.db.QueryRow(`SELECT author_id FROM message WHERE room_id=? AND body='from v1' AND author_id IN (?, 'sage@old') LIMIT 1`, tc.roomID, card.ID).Scan(&got); err != nil || got != tc.want {
			t.Fatalf("room %s: author=%q want=%q err=%v", tc.roomID, got, tc.want, err)
		}
	}
}

func TestStartFromLegacyRefusesBadNamesAndALinkAlreadyTaken(t *testing.T) {
	t.Parallel()
	s, _, room := legacyStartFixture(t)
	if _, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake", Slug: "Bad Name", ResumeLegacyID: "sage@old"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("invalid name: %v", err)
	}
	// "worker" is a role key, which @worker would clash with.
	if _, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake", Slug: "worker", ResumeLegacyID: "sage@old"}); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("role key name: %v", err)
	}
	// The link is taken even if the names differ: one agent per legacy agent.
	if _, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake", Slug: "sagea", ResumeLegacyID: "sage@old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake", Slug: "sageb", ResumeLegacyID: "sage@old"}); !errors.Is(err, ErrLegacyStarted) {
		t.Fatalf("a second agent from one legacy agent: %v", err)
	}
	// The same name as the first start's agent is still "already started", not a name clash: the first one holds the name because of it.
	if _, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake", Slug: "sagea", ResumeLegacyID: "sage@old"}); !errors.Is(err, ErrLegacyStarted) {
		t.Fatalf("a second start under the same name: %v", err)
	}
}

// A supported native harness offers its recorded session; one without a usable id starts fresh.
func TestSupportedLegacyHarnessesOfferNativeContinuation(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"claude-code", "codex", "pi", "opencode", "vibe"} {
		info := LegacyStart{Harness: harness, SessionID: "abc-12345678", CWD: "/work"}
		info.Tool = ResumableTool(harness)
		view := info.StartInfo(time.Now(), "/work")
		if !view.Resumable || view.Reason != "" {
			t.Fatalf("%s: %+v", harness, view)
		}
	}
	missing := LegacyStart{Harness: "vibe", Tool: "vibe", CWD: "/work"}
	if view := missing.StartInfo(time.Now(), "/work"); view.Resumable || !strings.Contains(view.Reason, "usable session id") {
		t.Fatalf("missing: %+v", view)
	}
}

func TestStartFromLegacyWithoutAResumableSessionStartsFreshUnderTheSameName(t *testing.T) {
	t.Parallel()
	s, _, room := legacyStartFixture(t)
	ctx := context.Background()
	info, _ := s.LegacyStart(ctx, room.ProjectID, "vib@old")
	if info.Tool != "vibe" {
		t.Fatalf("vibe maps to native continuation: %+v", info)
	}
	view := info.StartInfo(time.Now(), "/tmp/wake")
	if view.Resumable || !strings.Contains(view.Reason, "usable session id") {
		t.Fatalf("start info: %+v", view)
	}
	card, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake", Slug: "vib", ResumeLegacyID: "vib@old"})
	if err != nil || card.Slug != "vib" {
		t.Fatalf("fresh start: %+v %v", card, err)
	}
	var mode string
	var startedAt *string
	if err := s.db.QueryRow(`SELECT start_mode,native_session_started_at FROM session WHERE agent_id=?`, card.ID).Scan(&mode, &startedAt); err != nil || mode != "fresh_with_history" || startedAt != nil {
		t.Fatalf("session: mode=%q started=%v err=%v", mode, startedAt, err)
	}
	if got, _ := s.LegacyStart(ctx, room.ProjectID, "vib@old"); got.StartedID != card.ID {
		t.Fatalf("link: %+v", got)
	}
}

func TestStartFromLegacyNativeSessionRequestsPrivateCopy(t *testing.T) {
	t.Parallel()
	s, _, room := legacyStartFixture(t)
	if _, err := s.db.Exec(`UPDATE legacy_agent_session SET harness='codex',harness_session_id=? WHERE agent_id='vib@old'`, "0f7c6a1e-1111-4222-8333-444455556666"); err != nil {
		t.Fatal(err)
	}
	card, err := startFromLegacy(t, s, room, SpawnInput{Tool: "codex", Variant: "native", Model: "gpt-6-sol", Path: "/tmp/wake", Slug: "vib", ResumeLegacyID: "vib@old", ResumeHandle: "0f7c6a1e-1111-4222-8333-444455556666"})
	if err != nil {
		t.Fatal(err)
	}
	var copyNeeded bool
	if err := s.Tx(context.Background(), func(tx *Tx) error {
		var sessionID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM session WHERE agent_id=?`, card.ID).Scan(&sessionID); err != nil {
			return err
		}
		var err error
		copyNeeded, err = tx.importLegacySession(card.ID, sessionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !copyNeeded {
		t.Fatal("the first resumed native turn must stage its v1 session")
	}
	pending, err := s.AgentLegacyCopyPending(context.Background(), card.ID)
	if err != nil || !pending {
		t.Fatalf("supervisor capability hold: pending=%v err=%v", pending, err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_started_at=NULL WHERE agent_id=?`, card.ID); err != nil {
		t.Fatal(err)
	}
	pending, err = s.AgentLegacyCopyPending(context.Background(), card.ID)
	if err != nil || pending {
		t.Fatalf("new native session must not be held: pending=%v err=%v", pending, err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_started_at=? WHERE agent_id=?`, timestamp(), card.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?,?,1,'fresh_with_history',?)`, "ses_new", card.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	pending, err = s.AgentLegacyCopyPending(context.Background(), card.ID)
	if err != nil || pending {
		t.Fatalf("later generation must not be held: pending=%v err=%v", pending, err)
	}
}

func TestLegacyNativeCopySurvivesStopBeforeFirstTurn(t *testing.T) {
	for _, tool := range []string{"codex", "pi", "opencode", "vibe"} {
		t.Run(tool, func(t *testing.T) {
			s, _, room := legacyStartFixture(t)
			const handle = "0f7c6a1e-1111-4222-8333-444455556666"
			if _, err := s.db.Exec(`UPDATE legacy_agent_session SET harness=?,harness_session_id=? WHERE agent_id='vib@old'`, tool, handle); err != nil {
				t.Fatal(err)
			}
			card, err := startFromLegacy(t, s, room, SpawnInput{Tool: tool, Variant: "native", Model: "m", Path: "/tmp/wake", Slug: "vib", ResumeLegacyID: "vib@old", ResumeHandle: handle})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var stopped, restarted AgentCard
			if err := s.Tx(ctx, func(tx *Tx) error {
				stopped, err = tx.SetAgentDesired(card.ID, "usr_start", "stopped", card.DesiredRev)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, card.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.Tx(ctx, func(tx *Tx) error {
				restarted, err = tx.SetAgentDesired(card.ID, "usr_start", "active", stopped.DesiredRev)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			pending, err := s.AgentLegacyCopyPending(ctx, card.ID)
			if err != nil || !pending {
				t.Fatalf("copy hold after restart: pending=%v err=%v", pending, err)
			}
			message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", Body: "Continue"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES (?,?,?,?,'queued','mention',?)`, "note_legacy_restart", card.ID, message.ID, room.ID, timestamp()); err != nil {
				t.Fatal(err)
			}
			delivery, ready, err := s.PrepareDispatch(ctx, card.ID, "")
			if err != nil || !ready || delivery.Generation != restarted.Generation || !delivery.ImportLegacySession || delivery.NativeSessionHandle != handle {
				t.Fatalf("restarted delivery: %+v ready=%v err=%v", delivery, ready, err)
			}
			if err := s.AdmitTurn(ctx, Admission{TurnID: delivery.TurnID, AgentID: card.ID, MachineID: card.MachineID,
				Generation: restarted.Generation, DesiredRev: restarted.DesiredRev, NoteIDs: []string{"note_legacy_restart"}}); err != nil {
				t.Fatal(err)
			}
			if err := s.AckTurn(ctx, delivery.TurnID, card.ID, restarted.Generation, "accepted"); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProcess(ctx, delivery.TurnID, card.ID, restarted.Generation, 0, true, false); err != nil {
				t.Fatal(err)
			}
			pending, err = s.AgentLegacyCopyPending(ctx, card.ID)
			if err != nil || !pending {
				t.Fatalf("process exit alone does not prove a settled copy: pending=%v err=%v", pending, err)
			}
			raw := []byte(`{"messages":[]}`)
			parsed, err := answer.Parse(raw, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: delivery.TurnID, AgentID: card.ID, Generation: restarted.Generation,
				AnswerRev: 0, Digest: "legacy-copy-settled", Candidate: raw, Parsed: &parsed}); err != nil {
				t.Fatal(err)
			}
			pending, err = s.AgentLegacyCopyPending(ctx, card.ID)
			if err != nil || pending {
				t.Fatalf("completed turn satisfies copy hold: pending=%v err=%v", pending, err)
			}
			var copyNeeded bool
			if err := s.Tx(ctx, func(tx *Tx) error {
				var sessionID string
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT session_id FROM turn WHERE id=?`, delivery.TurnID).Scan(&sessionID); err != nil {
					return err
				}
				copyNeeded, err = tx.importLegacySession(card.ID, sessionID)
				return err
			}); err != nil || copyNeeded {
				t.Fatalf("completed turn must clear future copy deliveries: copy=%v err=%v", copyNeeded, err)
			}
			if err := s.Tx(ctx, func(tx *Tx) error {
				stopped, err = tx.SetAgentDesired(card.ID, "usr_start", "stopped", restarted.DesiredRev)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, card.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.Tx(ctx, func(tx *Tx) error {
				restarted, err = tx.SetAgentDesired(card.ID, "usr_start", "active", stopped.DesiredRev)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			pending, err = s.AgentLegacyCopyPending(ctx, card.ID)
			if err != nil || pending {
				t.Fatalf("completed copy must stay released after another restart: pending=%v err=%v", pending, err)
			}
		})
	}
}

// A session continued outside the project folder goes through the ordinary boundary approval, and says so.
func TestResumeOutsideTheProjectFolderWaitsForBoundaryApproval(t *testing.T) {
	t.Parallel()
	s, _, room := legacyStartFixture(t)
	ctx := context.Background()
	info, _ := s.LegacyStart(ctx, room.ProjectID, "far@old")
	if view := info.StartInfo(time.Now(), "/tmp/wake"); !view.Resumable || !view.OutsideFolder || view.V1Active {
		t.Fatalf("start info: %+v", view)
	}
	card, err := startFromLegacy(t, s, room, SpawnInput{Tool: "claude", Variant: "native", Model: "m", Path: "/elsewhere/dir", Slug: "far", ResumeLegacyID: "far@old", ResumeHandle: info.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if card.BoundaryPending == "" || card.Desired != "not_accepting" {
		t.Fatalf("an agent outside the folder must wait for approval: %+v", card)
	}
	// The stored notice names the agent by its @agt_<id> token, so the old slug is no longer in its text.
	var body string
	if err := s.db.QueryRow(`SELECT body FROM message WHERE room_id=? AND author_kind='system' AND body LIKE '%awaiting owner approval%' ORDER BY seq DESC LIMIT 1`, room.ID).Scan(&body); err != nil ||
		!strings.Contains(body, "outside the project folder") || !strings.Contains(body, "/elsewhere/dir") {
		t.Fatalf("notice: %q %v", body, err)
	}
}

func TestV1SeenRecentlyUsesAWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for last, recent := range map[string]bool{"2026-10-20T11:55:00Z": true, "2026-10-20T11:40:00Z": false, "2026-10-01T00:00:00Z": false, "": false, "garbage": false} {
		if got := (LegacyStart{LastSeen: last}).SeenRecently(now); got != recent {
			t.Fatalf("last_seen %q: %v", last, got)
		}
	}
	if !(LegacyStart{Tool: "claude", SessionID: "abcdef123456", LastSeen: "2026-10-20T11:59:00Z"}).StartInfo(now, "").V1Active {
		t.Fatal("a recently seen agent is flagged as possibly active in v1")
	}
}

// The lookup is per project: an agent that wrote in any room of the project is found, whichever room asks; one that wrote only in another
// project is not found at all.
func TestLegacyStartIsScopedToTheProjectTheAgentWroteIn(t *testing.T) {
	t.Parallel()
	s, project, room := legacyStartFixture(t)
	ctx := context.Background()
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "other", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == room.ID || other.ProjectID != room.ProjectID {
		t.Fatalf("expected a second room of the same project: %+v %+v", other, room)
	}
	if got, err := s.LegacyStart(ctx, other.ProjectID, "sage@old"); err != nil || got.AgentID != "sage@old" {
		t.Fatalf("an agent that wrote in another room of the project: %+v %v", got, err)
	}
	foreign, err := s.CreateProject(ctx, Project{Name: "foreign", Folder: "/tmp/foreign"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LegacyStart(ctx, foreign.ID, "sage@old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an agent that never wrote in the project: %v", err)
	}
	if _, err := s.LegacyStart(ctx, project.ID, "nobody@old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an unknown agent: %v", err)
	}
}

// The dialog must not promise a resume the server will not do: an id that is not a plausible session id is not resumable.
func TestStartInfoAppliesTheSameSessionIdCheckAsTheServer(t *testing.T) {
	t.Parallel()
	for handle, ok := range map[string]bool{"0f7c6a1e-1111-4222-8333-444455556666": true, "short": false, "--dangerous-option": false, "../etc/passwd": false, "abc def ghi jkl": false, "": false} {
		if ValidSessionHandle(handle) != ok {
			t.Fatalf("ValidSessionHandle(%q) != %v", handle, ok)
		}
		view := LegacyStart{Harness: "claude-code", Tool: "claude", SessionID: handle, CWD: "/work"}.StartInfo(time.Now(), "/work")
		if view.Resumable != ok {
			t.Fatalf("%q: resumable=%v, want %v (%s)", handle, view.Resumable, ok, view.Reason)
		}
	}
}

// A room's worktree is where only the agent that uses it works: a continued v1 session in its old folder does not show the worktree.
func TestProfileShowsTheWorktreeOnlyForTheAgentThatUsesIt(t *testing.T) {
	t.Parallel()
	s, project, room := legacyStartFixture(t)
	ctx := context.Background()
	card, err := startFromLegacy(t, s, room, SpawnInput{Path: "/tmp/wake/sub", Slug: "sage", ResumeLegacyID: "sage@old", ResumeHandle: "0f7c6a1e-1111-4222-8333-444455556666"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestRoomWorktree(ctx, room.ID, "main"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRoomWorktree(ctx, room.ID, "mch_wake", "/tmp/wake/.aimebu/worktrees/"+room.ID, "main", "base", "ready", "", 0); err != nil {
		t.Fatal(err)
	}
	profile, err := s.AgentProfile(ctx, room.ID, card.ID)
	if err != nil || profile.Worktree || profile.Branch != "" || profile.Cwd != "/tmp/wake/sub" {
		t.Fatalf("the continued agent works in its own folder: %+v %v", profile, err)
	}
	_ = project
}

// A private chat is not part of the project's history for this lookup: an agent whose only messages are in a private chat is not found,
// so someone who is not in that chat cannot start it (or learn it wrote there).
func TestLegacyStartIgnoresPrivateChats(t *testing.T) {
	t.Parallel()
	s, project, room := legacyStartFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO room(id,project_id,depth,kind,dm_key,title,created_by,created_at) VALUES ('rm_dm_only',?,0,'dm','dm:one:two','dm','owner','now')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE message SET room_id='rm_dm_only' WHERE author_id='sage@old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LegacyStart(ctx, project.ID, "sage@old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an agent with only private-chat history: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE message SET room_id=? WHERE author_id='sage@old'`, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LegacyStart(ctx, project.ID, "sage@old"); err != nil {
		t.Fatalf("back in a project room: %v", err)
	}
}

// The lookup goes through an index on the author and never reads the whole message table or every message of a project's rooms.
func TestLegacyStartQueryUsesTheAuthorIndex(t *testing.T) {
	t.Parallel()
	s, _, _ := legacyStartFixture(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+legacyStartQuery, "sage@old", "prj")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	usesIndex := false
	for _, step := range plan {
		if strings.Contains(step, "message_agent_author_room") {
			usesIndex = true
		}
		if strings.HasPrefix(step, "SCAN message") || strings.HasPrefix(step, "SCAN m") && !strings.Contains(step, "USING") {
			t.Fatalf("the lookup reads every message: %v", plan)
		}
	}
	if !usesIndex {
		t.Fatalf("the lookup does not use the author index: %v", plan)
	}
}

// v1 keeps one session per name. A name that another harness or model, or an earlier instance, used still holds that session; the
// history's messages say who wrote them, and a session that does not fit is neither offered nor shown as the sender's.
func TestLegacySessionThatDoesNotBelongToTheSenderIsNotOffered(t *testing.T) {
	t.Parallel()
	s, project, room := legacyStartFixture(t)
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO legacy_agent_session(agent_id,harness,model,project,cwd,harness_session_id,registered_at,last_seen) VALUES
			('hal@old','codex','gpt5','old','/tmp/wake','019fd868-aaaa-4bbb-8ccc-ddddeeeeffff','2026-08-03T10:00:00Z','2026-08-04T10:00:00Z'),
			('mod@old','claude-code','sonnet4.6','old','/tmp/wake','0f7c6a1e-2222-4222-8333-444455556666','2026-10-01T10:00:00Z','2026-10-26T10:00:00Z'),
			('late@old','claude-code','opus5.5','old','/tmp/wake','0f7c6a1e-3333-4222-8333-444455556666','2026-07-01T10:00:00Z','2026-07-02T10:00:00Z'),
			('fit@old','claude-code','opus5.5','old','/tmp/wake','0f7c6a1e-4444-4222-8333-444455556666','2026-10-01T10:00:00Z','2026-10-26T10:00:00Z'),
			('none@old','codex','gpt5','old','/tmp/wake','019fd868-bbbb-4bbb-8ccc-ddddeeeeffff','2026-08-03T10:00:00Z','2026-08-04T10:00:00Z')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	say := func(id, harness, model, at string, seq int) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at,author_harness,author_model) VALUES (?,?,?,'agent',?,'from v1','[]',?,?,?)`,
			"msg_"+id+at, seq, room.ID, id, at, harness, model); err != nil {
			t.Fatal(err)
		}
	}
	// hal: today's Claude Code messages, next to an old Codex session. mod: the same harness, another model. late: the session was last seen
	// weeks before the agent's first message. fit: everything agrees. none: messages that name no harness (older v1) prove nothing.
	say("hal@old", "claude-code", "opus5.5", "2026-10-26T09:00:00.000000000Z", 910001)
	say("mod@old", "claude-code", "opus5.5", "2026-10-26T09:00:00.000000000Z", 910002)
	say("late@old", "claude-code", "opus5.5", "2026-10-26T09:00:00.000000000Z", 910003)
	say("fit@old", "claude-code", "opus5.5", "2026-10-26T09:00:00.000000000Z", 910004)
	say("none@old", "", "", "2026-10-26T09:00:00.000000000Z", 910005)

	for _, id := range []string{"hal@old", "mod@old", "late@old"} {
		info, err := s.LegacyStart(ctx, room.ProjectID, id)
		if err != nil || info.Mismatch == "" || info.SessionID != "" || info.Tool != "" || info.Harness != "claude-code" || info.Model != "opus5.5" {
			t.Fatalf("%s: %+v %v, want the stray session dropped and the messages' harness and model shown", id, info, err)
		}
		start := info.StartInfo(time.Now(), "/tmp/wake")
		if start.Resumable || start.Tool != "claude" || !strings.Contains(start.Reason, "not this agent's") || start.CWD != "" {
			t.Fatalf("%s start info: %+v", id, start)
		}
		profile, err := s.LegacyAgentProfile(ctx, room.ID, id)
		if err != nil || profile.NativeSessionID != "" || profile.Cwd != "" {
			t.Fatalf("%s profile shows the stray session: %+v %v", id, profile, err)
		}
	}
	if info, _ := s.LegacyStart(ctx, room.ProjectID, "hal@old"); !strings.Contains(info.Mismatch, "codex") {
		t.Fatalf("the reason does not name the stray harness: %q", info.Mismatch)
	}
	for _, id := range []string{"fit@old", "none@old"} {
		info, err := s.LegacyStart(ctx, room.ProjectID, id)
		if err != nil || info.Mismatch != "" || info.SessionID == "" {
			t.Fatalf("%s: %+v %v, want its session kept", id, info, err)
		}
	}
	if profile, err := s.LegacyAgentProfile(ctx, room.ID, "fit@old"); err != nil || profile.NativeSessionID == "" {
		t.Fatalf("fit profile: %+v %v", profile, err)
	}
	_ = project
}

// A v1 build that stamps each message with its session says exactly which session did a sender's work, and keeps every session of a name.
func TestLegacyStartOffersTheSessionOfTheSendersLatestMessage(t *testing.T) {
	t.Parallel()
	s, _, room := legacyStartFixture(t)
	ctx := context.Background()
	const older, latest, unknown = "0f7c6a1e-aaaa-4222-8333-444455556666", "0f7c6a1e-bbbb-4222-8333-444455556666", "0f7c6a1e-cccc-4222-8333-444455556666"
	for _, statement := range []string{
		// The name's latest session (what agent_sessions kept) is a newer instance's; the sender's messages came from the older one.
		`INSERT INTO legacy_agent_session(agent_id,harness,model,project,cwd,harness_session_id,registered_at,last_seen) VALUES
			('who@old','claude-code','opus5.5','old','/tmp/wake/newer','` + latest + `','2026-10-01T10:00:00Z','2026-10-26T10:00:00Z'),
			('lost@old','claude-code','opus5.5','old','/tmp/wake/newer','` + latest + `','2026-10-01T10:00:00Z','2026-10-26T10:00:00Z')`,
		`INSERT INTO legacy_agent_session_history(agent_id,harness_session_id,harness,model,project,cwd,origin,registered_at,last_seen) VALUES
			('who@old','` + older + `','claude-code','opus4.5','old','/tmp/wake/sub','wrapper','2026-10-01T10:00:00Z','2026-10-10T10:00:00Z'),
			('who@old','` + latest + `','claude-code','opus5.5','old','/tmp/wake/newer','wrapper','2026-10-20T10:00:00Z','2026-10-26T10:00:00Z')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	say := func(id, session, at string, seq int) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at,author_harness,author_model,author_session) VALUES (?,?,?,'agent',?,'from v1','[]',?,'claude-code','opus4.5',?)`,
			"msg_"+id+session, seq, room.ID, id, at, session); err != nil {
			t.Fatal(err)
		}
	}
	say("who@old", older, "2026-10-09T09:00:00.000000000Z", 920001)
	say("lost@old", unknown, "2026-10-09T09:00:00.000000000Z", 920002)

	info, err := s.LegacyStart(ctx, room.ProjectID, "who@old")
	if err != nil || info.SessionID != older || info.Mismatch != "" || info.CWD != "/tmp/wake/sub" || info.Model != "opus4.5" || info.Tool != "claude" {
		t.Fatalf("who: %+v %v, want the older session with its own folder and model", info, err)
	}
	// A session the history has no row for cannot be continued (no folder), and the name's latest session is not offered in its place.
	lost, err := s.LegacyStart(ctx, room.ProjectID, "lost@old")
	if err != nil || lost.SessionID != "" || lost.Tool != "" || !strings.Contains(lost.Mismatch, unknown) {
		t.Fatalf("lost: %+v %v", lost, err)
	}
	// The profile names the same session the dialog offers, not the name's latest record: two sessions of one harness and model are
	// not told apart by the contradiction checks, only by the stamp.
	if profile, err := s.LegacyAgentProfile(ctx, room.ID, "who@old"); err != nil || profile.NativeSessionID != older || profile.Cwd != "/tmp/wake/sub" {
		t.Fatalf("who profile = %+v %v, want the older session and its folder", profile, err)
	}
	if profile, err := s.LegacyAgentProfile(ctx, room.ID, "lost@old"); err != nil || profile.NativeSessionID != "" || profile.Cwd != "" {
		t.Fatalf("a stamped session that cannot be verified is not shown: %+v %v", profile, err)
	}
	// When the latest session sent the latest message, the name's own record is used as it is.
	say("who@old", latest, "2026-10-26T09:00:00.000000000Z", 920003)
	if again, err := s.LegacyStart(ctx, room.ProjectID, "who@old"); err != nil || again.SessionID != latest || again.CWD != "/tmp/wake/newer" {
		t.Fatalf("after the newer message: %+v %v", again, err)
	}
}

// An imported agent is started where its history is: the room of the project with most of its messages (a private chat excluded, the newest
// one winning a tie), which is named with its kind and state so the dialog can start there, or say why it cannot.
func TestLegacyStartNamesTheRoomThatHoldsTheHistory(t *testing.T) {
	s, project, room := legacyStartFixture(t)
	ctx := t.Context()
	start, err := s.LegacyStart(ctx, project.ID, "sage@old")
	if err != nil || start.HistoryRoom.ID != room.ID || start.HistoryRoom.Kind == "" || start.HistoryRoom.Lifecycle != "open" {
		t.Fatalf("one message in one room: %+v %v", start.HistoryRoom, err)
	}
	var other Room
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		other, err = tx.CreateRoom(Room{ProjectID: project.ID, ParentID: room.ID, Kind: "task", Title: "archive", CreatedBy: "usr_start"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	insert := func(id string, seq int, roomID string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES (?,?,?,'agent','sage@old','more v1','[]','2026-10-02T09:00:00.000000000Z')`, id, seq, roomID); err != nil {
			t.Fatal(err)
		}
	}
	insert("msg_h1", 910001, other.ID)
	insert("msg_h2", 910002, other.ID)
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, other.ID); err != nil {
		t.Fatal(err)
	}
	start, err = s.LegacyStart(ctx, project.ID, "sage@old")
	if err != nil || start.HistoryRoom.ID != other.ID || start.HistoryRoom.Title != "archive" || start.HistoryRoom.Lifecycle != "closed" {
		t.Fatalf("the room with most messages, archived: %+v %v", start.HistoryRoom, err)
	}
	info := start.StartInfo(time.Now(), "")
	if info.HistoryRoom == nil || info.HistoryRoom.ID != other.ID {
		t.Fatalf("the dialog is told: %+v", info.HistoryRoom)
	}
	// A tie goes to the room with the newest message, and a private chat never counts.
	insert("msg_h3", 910003, room.ID)
	insert("msg_h4", 910004, room.ID)
	start, err = s.LegacyStart(ctx, project.ID, "sage@old")
	if err != nil || start.HistoryRoom.ID != room.ID {
		t.Fatalf("a tie goes to the newest: %+v %v", start.HistoryRoom, err)
	}
}
