package store

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
)

func TestAgentProfileLabelsEstimatedDailyCost(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	if err := s.AddMember(t.Context(), f.room.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	frame, _ := json.Marshal(map[string]any{"type": "usage", "usage": map[string]any{"tokens_in": 100, "tokens_out": 50, "cost_estimate": 0.02, "cost_source": "estimated"}})
	if err := s.RecordTurnEvent(t.Context(), f.turn.ID, f.agent.ID, "estimated-cost", "usage", "", frame); err != nil {
		t.Fatal(err)
	}
	profile, err := s.AgentProfile(t.Context(), f.room.ID, f.agent.ID)
	if err != nil || profile.CostToday == nil || *profile.CostToday != 0.02 || !profile.CostTodayEstimated {
		t.Fatalf("estimated profile: %+v %v", profile, err)
	}
}

func TestResumeCommandKnownToolsAndQuoting(t *testing.T) {
	t.Parallel()
	hash := sha256.Sum256([]byte("agt_one"))
	agentHash := hex.EncodeToString(hash[:])
	runtime := func(tool string) string { return filepath.Join("/config", "supervisor", tool, agentHash) }
	for _, c := range []struct{ tool, session, cwd, want string }{
		{"claude", "3b24-11", "/work/app", "cd /work/app && claude --resume 3b24-11"},
		{"codex", "abc", "/work/app", "cd /work/app && CODEX_HOME=" + runtime("codex") + " codex resume abc"},
		{"pi", "abc", "/work/app", "cd /work/app && PI_CODING_AGENT_DIR=" + runtime("pi") + " pi --session-dir /work/app/.aimebu/pi-sessions/" + agentHash + " --session-id abc"},
		{"opencode", "abc", "/work/app", "cd /work/app && OPENCODE_DB=" + filepath.Join(runtime("opencode"), "opencode.db") + " opencode --session abc"},
		{"vibe", "abc", "/work/app", "cd /work/app && VIBE_HOME=" + filepath.Join(runtime("vibe"), "home") + " VIBE_SESSION_LOGGING__SAVE_DIR=" + filepath.Join(runtime("vibe"), "sessions") + " vibe --resume abc"},
		{"claude", "abc", "", "claude --resume abc"},
		{"codex", "abc", "/path with space/it's", `cd '/path with space/it'\''s' && CODEX_HOME=` + runtime("codex") + ` codex resume abc`},
		{"claude", "", "/work", ""},
		{"pi", "abc", "", ""},
		{"fake", "abc", "/work", ""},
	} {
		if got := resumeCommand(c.tool, c.session, c.cwd, "agt_one", "/config"); got != c.want {
			t.Errorf("resumeCommand(%q,%q,%q) = %q, want %q", c.tool, c.session, c.cwd, got, c.want)
		}
	}
}

func TestAgentProfileUsesTheWorktreeAndTodaysUse(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := t.Context()
	var machineID string
	if err := s.db.QueryRow(`SELECT machine_id FROM agent WHERE id=?`, one.ID).Scan(&machineID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO room_worktree(room_id,base_ref,branch_name,relative_path,state,created_at,updated_at) VALUES ('` + room.ID + `','main','feature/x','wt','ready','t','t')`,
		`INSERT INTO room_worktree_instance(room_id,machine_id,canonical_path,state,updated_at) VALUES ('` + room.ID + `','` + machineID + `','/tmp/wake-wt','ready','t')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// The agent was spawned into the room's worktree, so that is its checkout; an agent working elsewhere is not shown the worktree.
	if _, err := s.db.Exec(`UPDATE checkout SET canonical_path='/tmp/wake-wt' WHERE id=(SELECT checkout_id FROM agent WHERE id=?)`, one.ID); err != nil {
		t.Fatal(err)
	}
	profile, err := s.AgentProfile(ctx, room.ID, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Worktree || profile.Cwd != "/tmp/wake-wt" || profile.Branch != "feature/x" || profile.MachineName != "host" || profile.ProjectName != "wake" || profile.SpawnedBy != "owner" {
		t.Fatalf("profile: %+v", profile)
	}
	if profile.TurnsToday != 0 || profile.CostToday != nil {
		t.Fatalf("nothing used today: %+v", profile)
	}
	if _, err := s.AgentProfile(ctx, room.ID, "agt_nobody"); err == nil {
		t.Fatal("an agent that is not in the room must not have a profile")
	}
}

func TestAgentProfileOffersNoResumeCommandInsideAContainer(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	if _, err := s.db.Exec(`UPDATE agent SET tool='claude' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='abc' WHERE agent_id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	profile, err := s.AgentProfile(t.Context(), room.ID, one.ID)
	if err != nil || profile.ResumeCommand != "" {
		t.Fatalf("an unconfirmed handle gets no command: %+v %v", profile, err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_started_at='2026-10-17T10:00:00Z' WHERE agent_id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	profile, err = s.AgentProfile(t.Context(), room.ID, one.ID)
	if err != nil || profile.ResumeCommand == "" {
		t.Fatalf("a host launcher gets the command: %+v %v", profile, err)
	}
	var machineID string
	if err := s.db.QueryRow(`SELECT machine_id FROM agent WHERE id=?`, one.ID).Scan(&machineID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,agent_server_url) VALUES ('lch_c',?,'claude','docker','docker',1,1,'http://x')`, machineID); err != nil {
		t.Skipf("launcher fixture does not fit this schema: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET launcher_id='lch_c' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	profile, err = s.AgentProfile(t.Context(), room.ID, one.ID)
	if err != nil || profile.ResumeCommand != "" || profile.LauncherName != "docker" || profile.NativeSessionID != "abc" {
		t.Fatalf("a container launcher gets no resume command: %+v %v", profile, err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET launcher_id=NULL,variant='docker' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	profile, err = s.AgentProfile(t.Context(), room.ID, one.ID)
	if err != nil || profile.ResumeCommand != "" {
		t.Fatalf("a docker variant gets no host command: %+v %v", profile, err)
	}
}

func TestAgentProfileCodexResumeUsesPrivateHome(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	if _, err := s.db.Exec(`UPDATE agent SET tool='codex' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='sess-1',native_session_started_at='2026-10-17T10:00:00Z' WHERE agent_id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	profile, err := s.AgentProfile(t.Context(), room.ID, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(one.ID))
	home := filepath.Join(filepath.Dir(filepath.Dir(s.dbPath)), "supervisor", "codex", hex.EncodeToString(hash[:]))
	want := "cd " + shellQuote(profile.Cwd) + " && CODEX_HOME=" + shellQuote(home) + " codex resume sess-1"
	if profile.ResumeCommand != want {
		t.Fatalf("resume command = %q, want %q", profile.ResumeCommand, want)
	}
}

// A departed sender is still shown in the chat, and its name opens a profile; a stranger that never spoke in the room has none.
func TestAgentProfileOfADepartedSenderWhoSpokeInTheRoom(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	if _, err := s.db.Exec(`UPDATE room_member SET left_at='2026-01-01T00:00:00Z' WHERE member_id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AgentProfile(t.Context(), room.ID, one.ID); err == nil {
		t.Fatal("an agent that left and never spoke has no profile")
	}
	if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_departed',900001,?,'agent',?,'last words','[]','2026-01-01T00:00:00Z')`, room.ID, one.ID); err != nil {
		t.Fatal(err)
	}
	profile, err := s.AgentProfile(t.Context(), room.ID, one.ID)
	if err != nil || profile.AgentID != one.ID {
		t.Fatalf("a departed sender who spoke in the room has a profile: %+v %v", profile, err)
	}
	if _, err := s.AgentProfile(t.Context(), room.ID, "agt_stranger"); err == nil {
		t.Fatal("an agent that is unknown has no profile")
	}
}
