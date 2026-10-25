package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHarnessNamesMapToTheToolsThatCanResume(t *testing.T) {
	t.Parallel()
	for harness, tool := range map[string]string{"claude-code": "claude", "Claude": "claude", "codex": "", "pi": "", "opencode": "", "vibe": "", "cursor": "", "aider": "", "": ""} {
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

// A harness that cannot resume still gets its name and its link, with a fresh session.
// codex, pi, opencode and vibe run with a private home per agent in v2, so their v1 sessions cannot be continued; the dialog says so in words.
func TestOnlyClaudeSessionsCanBeContinuedAndTheOthersSayWhy(t *testing.T) {
	t.Parallel()
	for harness, wantReason := range map[string]string{"codex": "v2 keeps codex sessions per agent", "pi": "v2 keeps pi sessions per agent", "opencode": "v2 keeps opencode sessions per agent", "vibe": "v2 keeps vibe sessions per agent"} {
		info := LegacyStart{Harness: harness, SessionID: "abc-12345678", CWD: "/work"}
		info.Tool = ResumableTool(harness)
		view := info.StartInfo(time.Now(), "/work")
		if view.Resumable || !strings.Contains(view.Reason, wantReason) {
			t.Fatalf("%s: %+v", harness, view)
		}
	}
	claude := LegacyStart{Harness: "claude-code", Tool: ResumableTool("claude-code"), SessionID: "abc-12345678", CWD: "/work"}
	if view := claude.StartInfo(time.Now(), "/work"); !view.Resumable || view.Reason != "" {
		t.Fatalf("claude: %+v", view)
	}
}

func TestStartFromLegacyWithoutAResumableSessionStartsFreshUnderTheSameName(t *testing.T) {
	t.Parallel()
	s, _, room := legacyStartFixture(t)
	ctx := context.Background()
	info, _ := s.LegacyStart(ctx, room.ProjectID, "vib@old")
	if info.Tool != "" {
		t.Fatalf("vibe cannot resume: %+v", info)
	}
	view := info.StartInfo(time.Now(), "/tmp/wake")
	if view.Resumable || !strings.Contains(view.Reason, "cannot be continued") {
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
	var body string
	if err := s.db.QueryRow(`SELECT body FROM message WHERE room_id=? AND author_kind='system' AND body LIKE '%far%' ORDER BY seq DESC LIMIT 1`, room.ID).Scan(&body); err != nil ||
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
