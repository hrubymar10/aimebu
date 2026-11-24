package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

func TestProviderLimitKnownFailureGatesWithoutRecovery(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	info := adapter.ExitInfo{ExitCode: 1, ErrorEvent: fmt.Sprintf(`{"type":"turn.failed","message":"usage limit reached; resets at %s"}`, future.Format(time.RFC3339))}
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, info); err != nil {
		t.Fatal(err)
	}
	var outcome, settle, noteState string
	var blocked bool
	if err := s.db.QueryRow(`SELECT outcome,settle FROM turn WHERE id=?`, f.turn.ID).Scan(&outcome, &settle); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&noteState); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if outcome != "failed" || settle != "settled" || noteState != "queued" || blocked {
		t.Fatalf("turn=%s/%s note=%s recovery=%t", outcome, settle, noteState, blocked)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].Kind != "usage_info" || !strings.Contains(items[0].Question, future.Format("2006-01-02T15:04:05")) {
		t.Fatalf("inbox=%+v error=%v", items, err)
	}
	flags, err := s.AgentHealth(ctx, f.agent.ID, time.Now(), 10*time.Minute)
	if err != nil || len(flags) != 1 || flags[0].Code != "out_of_quota" || flags[0].ResetAt != future.Format("2006-01-02T15:04:05.000000000Z") {
		t.Fatalf("flags=%+v error=%v", flags, err)
	}
	_, err = s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner, Body: "new request"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("quota dispatch ready=%t error=%v", ready, err)
	}
	if _, err := s.db.Exec(`UPDATE provider_limit SET reset_at=? WHERE agent_id=?`, time.Now().Add(-time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z"), f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceProviderLimits(ctx); err != nil {
		t.Fatal(err)
	}
	items, err = s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("resolved inbox=%+v error=%v", items, err)
	}
	if delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || !ready || len(delivery.Notes) != 2 || delivery.Notes[0].RetryNotice != "" {
		t.Fatalf("reset dispatch=%+v ready=%t error=%v", delivery, ready, err)
	}
}

func TestProviderLimitUnknownResetClearsOnReactivate(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, StderrTail: "Codex usage limit"}); err != nil {
		t.Fatal(err)
	}
	flags, err := s.AgentHealth(ctx, f.agent.ID, time.Now(), 10*time.Minute)
	if err != nil || len(flags) != 1 || flags[0].Code != "out_of_quota" || flags[0].ResetAt != "" {
		t.Fatalf("flags=%+v error=%v", flags, err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, "usr_recovery", "active", 0); return err }); err != nil {
		t.Fatal(err)
	}
	flags, err = s.AgentHealth(ctx, f.agent.ID, time.Now(), 10*time.Minute)
	if err != nil || len(flags) != 0 {
		t.Fatalf("cleared flags=%+v error=%v", flags, err)
	}
}

func TestProviderLimitRetryNowPreservesActionCaution(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, "tool-started", "activity", "item.started", []byte(`{"type":"activity"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, ErrorEvent: `{"type":"turn.failed","message":"usage limit reached"}`}); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || len(items[0].Options) != 1 || items[0].Options[0] != "Retry now" {
		t.Fatalf("retry item=%+v error=%v", items, err)
	}
	decision, reused, err := s.DecideProviderLimit(ctx, items[0].ID, owner, "retry-key", "retry", int(items[0].Rev))
	if err != nil || reused || decision.Choice != "retry" {
		t.Fatalf("decision=%+v reused=%t error=%v", decision, reused, err)
	}
	if replay, reused, err := s.DecideProviderLimit(ctx, items[0].ID, owner, "retry-key", "retry", int(items[0].Rev)); err != nil || !reused || replay.ItemID != decision.ItemID {
		t.Fatalf("decision replay=%+v reused=%t error=%v", replay, reused, err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || len(delivery.Notes) != 1 || !strings.Contains(delivery.Notes[0].RetryNotice, "Check the state before acting") {
		t.Fatalf("cautious retry=%+v ready=%t error=%v", delivery, ready, err)
	}
}

func TestProviderLimitRequiresProviderSpecificProcessEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		tool, stderr, event string
		limited             bool
	}{
		{"codex", "usage limit reached; resets in 3 hours", "", true},
		{"claude", "limit reached; reset_at: 2026-10-15T12:00:00Z", "", true},
		{"fake", "usage limit reached", "", false},
		{"codex", "rate limit from a tool", "", false},
		{"claude", "rate limit reached; retry soon", "", false},
		{"claude", "usage quota exceeded", "", false},
		{"pi", "", "Pi provider error: 429 you have reached your weekly usage limit; try again in 3 hours", true},
		{"pi", "", "Pi provider error: 429 rate limit exceeded, please slow down", false},
		{"pi", "", "Pi provider error: 503 service unavailable", false},
		{"pi", "usage limit reached", "", false},
		{"opencode", "", "OpenCode error: APIError (HTTP 429): you have reached your weekly usage limit; try again in 3 hours", true},
		{"opencode", "", "OpenCode error: APIError (HTTP 429): rate limit reached, slow down", false},
		{"opencode", "", "OpenCode error: UnknownError: Unexpected server error", false},
		{"opencode", "usage limit reached", "", false},
		{"vibe", "", "Vibe error: API error from mistral (HTTP 429): you have reached your weekly usage limit; try again in 3 hours", true},
		{"vibe", "", "Vibe error: API error from mistral (HTTP 429): rate limit reached, slow down", false},
		{"vibe", "", "Vibe stopped the turn: <vibe_stop_event>Turn limit of 2 reached</vibe_stop_event>", false},
		{"vibe", "usage limit reached", "", false},
	} {
		_, limited := providerLimitEvidence(test.tool, test.stderr, test.event, now)
		if limited != test.limited {
			t.Fatalf("tool=%s stderr=%q event=%q limited=%t", test.tool, test.stderr, test.event, limited)
		}
	}
	reset, limited := providerLimitEvidence("pi", "", "Pi provider error: usage limit reached, try again in 3 hours", now)
	if !limited || reset != "2026-10-14T15:00:00.000000000Z" {
		t.Fatalf("pi reset=%q limited=%t", reset, limited)
	}
}

func TestAgentHealthStuckAndExistingInboxFlags(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE turn SET exec='running',started_at=? WHERE id=?`, time.Now().Add(-11*time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z"), f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,created_at)
		VALUES ('inb_health_recovery',?,?,?,?,? ,?,'recovery',?)`, f.project.ID, f.room.ID, f.turn.ID, f.agent.ID, "agent", f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,created_at)
		VALUES ('inb_health_held',?,?,?,?,? ,?,'delivery_failed',?)`, f.project.ID, f.room.ID, f.turn.ID, f.agent.ID, "agent", f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	flags, err := s.AgentHealth(ctx, f.agent.ID, time.Now(), 10*time.Minute)
	if err != nil || len(flags) != 3 || flags[0].Code != "stuck" || flags[1].Code != "held_answer" || flags[2].Code != "recovery_waiting" {
		t.Fatalf("flags=%+v error=%v", flags, err)
	}
	if _, err := s.db.Exec(`INSERT INTO event(seq,kind,project_id,room_id,agent_id,turn_id,payload_json,created_at)
		VALUES (99999,'supervisor_activity',?,?,?,?,?,?)`, f.project.ID, f.room.ID, f.agent.ID, f.turn.ID, `{}`, timestamp()); err != nil {
		t.Fatal(err)
	}
	flags, err = s.AgentHealth(ctx, f.agent.ID, time.Now(), 10*time.Minute)
	if err != nil || len(flags) != 2 || flags[0].Code == "stuck" {
		t.Fatalf("fresh activity flags=%+v error=%v", flags, err)
	}
}

// A usage limit from every tool that routes into the provider-limit path must
// be recordable: the table once admitted only claude and codex, so a Pi limit
// failed RecordProcessInfo with a CHECK violation and the exit was lost.
func TestProviderLimitIsRecordedForEveryToolWithLimitWording(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ tool, event string }{
		{"claude", `{"type":"result","is_error":true,"result":"limit reached; resets in 3 hours"}`},
		{"codex", `{"type":"turn.failed","message":"usage limit reached; resets in 3 hours"}`},
		{"pi", "Pi provider error: 429 you have reached your weekly usage limit; resets in 3 hours"},
		{"opencode", "OpenCode error: APIError (HTTP 429): you have reached your weekly usage limit; resets in 3 hours"},
	} {
		tc := tc
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			s, f, owner := recoveryFixture(t, false)
			ctx := context.Background()
			if _, err := s.db.Exec(`UPDATE agent SET tool=? WHERE id=?`, tc.tool, f.agent.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, ErrorEvent: tc.event}); err != nil {
				t.Fatalf("%s: a provider limit must be recorded, not rejected: %v", tc.tool, err)
			}
			var recorded string
			if err := s.db.QueryRow(`SELECT tool FROM provider_limit WHERE agent_id=?`, f.agent.ID).Scan(&recorded); err != nil || recorded != tc.tool {
				t.Fatalf("%s: provider_limit row %q: %v", tc.tool, recorded, err)
			}
			if items, err := s.InboxForUser(ctx, owner); err != nil || len(items) != 1 || items[0].Kind != "usage_info" {
				t.Fatalf("%s: expected the informational limit item and no recovery: %+v %v", tc.tool, items, err)
			}
		})
	}
}

func TestMigrationFortySixKeepsExistingProviderLimits(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	if err := s.RecordProcessInfo(context.Background(), f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, ErrorEvent: `{"type":"turn.failed","message":"usage limit reached; resets in 3 hours"}`}); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM provider_limit`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("limit row: %d %v", rows, err)
	}
	if _, err := s.db.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,tool) VALUES (?,?,?,'opencode')
		ON CONFLICT(agent_id) DO UPDATE SET tool=excluded.tool`, f.agent.ID, f.turn.ID, timestamp()); err != nil {
		t.Fatalf("opencode must be an accepted limit tool: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE provider_limit SET tool='fake' WHERE agent_id=?`, f.agent.ID); err == nil {
		t.Fatal("an unknown tool must still be rejected")
	}
}

// Unlike the test above, this one starts from a real version 45 table, so it
// fails if the table rebuild loses or mangles a row.
func TestMigrationFortySixRebuildKeepsRowsAndWidensTool(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "from-45.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:45] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
	}
	// The rows reference an agent and turn that this bare schema does not
	// need; foreign keys stay off so the test pins only the table rebuild.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,reset_at,tool) VALUES ('agt_old','turn_old','2026-10-16T00:00:00Z','2026-10-17T00:00:00Z','codex')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,tool) VALUES ('agt_pi','turn_pi','2026-10-16T00:00:00Z','pi')`); err == nil {
		t.Fatal("before migration 46 a Pi limit should be rejected")
	}
	if _, err := db.Exec(migrations[45].sql); err != nil {
		t.Fatalf("migration 46: %v", err)
	}
	var tool, reset string
	if err := db.QueryRow(`SELECT tool,reset_at FROM provider_limit WHERE agent_id='agt_old'`).Scan(&tool, &reset); err != nil || tool != "codex" || reset != "2026-10-17T00:00:00Z" {
		t.Fatalf("existing limit lost: tool=%q reset=%q err=%v", tool, reset, err)
	}
	for _, name := range []string{"pi", "opencode", "claude"} {
		if _, err := db.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,tool) VALUES (?, 'turn', 'now', ?)`, "agt_"+name, name); err != nil {
			t.Fatalf("tool %s after migration: %v", name, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,tool) VALUES ('agt_bad','turn','now','fake')`); err == nil {
		t.Fatal("an unknown tool must still be rejected")
	}
}

// Migration 83 lets a usage-limit hold be recorded for a Vibe agent, and keeps the holds that exist.
func TestMigrationEightyThreeAddsVibeToTheProviderLimitTools(t *testing.T) {
	t.Parallel()
	db := databaseAtVersion(t, filepath.Join(t.TempDir(), "from-82.sqlite"), 82)
	defer db.Close()
	db.SetMaxOpenConns(1)                                         // one connection, so the pragma below holds for every statement
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil { // the rows name an agent and a turn that do not exist
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,reset_at,tool) VALUES ('agt_old','turn_old','2026-10-16T00:00:00Z','2026-10-17T00:00:00Z','opencode')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,tool) VALUES ('agt_vibe','turn_vibe','2026-10-16T00:00:00Z','vibe')`); err == nil {
		t.Fatal("before migration 83 a Vibe limit should be rejected")
	}
	if err := migrateUpTo(context.Background(), db, 0); err != nil {
		t.Fatal(err)
	}
	var tool, reset string
	if err := db.QueryRow(`SELECT tool,reset_at FROM provider_limit WHERE agent_id='agt_old'`).Scan(&tool, &reset); err != nil || tool != "opencode" || reset != "2026-10-17T00:00:00Z" {
		t.Fatalf("existing limit lost: tool=%q reset=%q err=%v", tool, reset, err)
	}
	for _, name := range []string{"vibe", "pi", "claude"} {
		if _, err := db.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,tool) VALUES (?, 'turn', 'now', ?)`, "agt_"+name, name); err != nil {
			t.Fatalf("tool %s after migration: %v", name, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,tool) VALUES ('agt_bad','turn','now','fake')`); err == nil {
		t.Fatal("an unknown tool must still be rejected")
	}
}

// The agents at a provider limit now are listed for their machine (a cache ping would only meet the limit): one with a reset time in the future, or
// none yet, is; one whose reset has passed, and one on another machine, are not.
func TestProviderLimitedAgentsListsOnlyLimitsInForce(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	var machine string
	if err := s.db.QueryRow(`SELECT machine_id FROM agent WHERE id=?`, f.agent.ID).Scan(&machine); err != nil {
		t.Fatal(err)
	}
	var tool string
	if err := s.db.QueryRow(`SELECT tool FROM agent WHERE id=?`, f.agent.ID).Scan(&tool); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ProviderLimitedAgents(ctx, machine); err != nil || len(got) != 0 {
		t.Fatalf("no limit: %v %v", got, err)
	}
	stamp := func(d time.Duration) string { return time.Now().Add(d).UTC().Format("2006-01-02T15:04:05.000000000Z") }
	insert := func(reset any) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT OR REPLACE INTO provider_limit(agent_id,turn_id,detected_at,reset_at,tool) VALUES (?,?,?,?,?)`, f.agent.ID, f.turn.ID, stamp(-time.Hour), reset, tool); err != nil {
			t.Fatal(err)
		}
	}
	for name, reset := range map[string]any{"a reset in the future": stamp(time.Hour), "no reset time yet": nil} {
		insert(reset)
		if got, err := s.ProviderLimitedAgents(ctx, machine); err != nil || len(got) != 1 || got[0] != f.agent.ID {
			t.Fatalf("%s: %v %v", name, got, err)
		}
	}
	insert(stamp(-time.Minute))
	if got, err := s.ProviderLimitedAgents(ctx, machine); err != nil || len(got) != 0 {
		t.Fatalf("a limit that has reset: %v %v", got, err)
	}
	insert(stamp(time.Hour))
	if got, err := s.ProviderLimitedAgents(ctx, "mch_elsewhere"); err != nil || len(got) != 0 {
		t.Fatalf("another machine: %v %v", got, err)
	}
}
