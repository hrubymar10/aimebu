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
