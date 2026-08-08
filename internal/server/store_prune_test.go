package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/types"
)

// pruneCounts is a snapshot of what a prune leaves behind, used to compare the
// online (DELETE /all) and offline (PruneDataDir) paths against each other.
type pruneCounts struct {
	rooms, messages, agents, sessions, reactions, attachments int
}

func reloadPruneCounts(t *testing.T, dir string) pruneCounts {
	t.Helper()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var c pruneCounts
	s.mu.RLock()
	c.rooms = len(s.rooms)
	for _, rm := range s.messages {
		c.messages += len(rm)
	}
	c.agents = len(s.agents)
	c.sessions = len(s.agentSessions)
	s.mu.RUnlock()
	s.reactionsMu.RLock()
	c.reactions = len(s.reactions)
	s.reactionsMu.RUnlock()
	s.attachmentsMu.RLock()
	c.attachments = len(s.attachments)
	s.attachmentsMu.RUnlock()
	return c
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dstPath, data, info.Mode())
	})
	if err != nil {
		t.Fatalf("copyDir %s -> %s: %v", src, dst, err)
	}
}

// newStoreWithServer builds a store against a specific dir (not a fresh
// t.TempDir like setupTestServer) and serves its handlers, so the online prune
// path (DELETE /all) can run against a fixture loaded from disk.
func newStoreWithServer(t *testing.T, dir string) (*store, *httptest.Server) {
	t.Helper()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	setupHandlers(mux, s, BuildInfo{}, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

// buildPruneFixture seeds a dir with one room, two messages, one AI agent, one
// agent_session, one reaction, and one attachment — a conversation that must
// survive plain prune and be wiped by prune -a.
func buildPruneFixture(t *testing.T, dir string) {
	t.Helper()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ai, _, err := s.registerAIWithSession("gpt5", "codex", "test", nil, "alf", &types.AgentSession{
		HarnessSessionID: "s-1",
		RegisteredAt:     time.Now().UTC(),
		LastSeen:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", ai.ID); err != nil {
		t.Fatal(err)
	}
	m1, err := s.roomSend("general", ai.ID, "one", false, nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.roomSend("general", ai.ID, "two", false, nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	// A reaction on m1.
	s.reactionsMu.Lock()
	s.reactions[m1] = []types.Reaction{{AgentID: ai.ID, Emoji: "👍", CreatedAt: time.Now().UTC().Format(time.RFC3339)}}
	s.persistReactionsLocked()
	s.reactionsMu.Unlock()
	// An attachment (registry entry + blob file).
	if _, err := s.addAttachment("a.png", "image/png", testPNG(t, 1, 1), 1, 1); err != nil {
		t.Fatal(err)
	}
}

// TestPruneOnlineOfflineIdenticalEndState is the load-bearing item-4 test: the
// online (DELETE /all) and offline (PruneDataDir) prune paths must produce
// identical on-disk end states. The divergence between them is the most likely
// cause of the 2026-08-07 data loss. Both paths route through clearAll, so this
// asserts equivalence against the SAME fixture rather than each vs its own
// expectations — a unit test that passes both sides independently would not
// catch a divergence.
func TestPruneOnlineOfflineIdenticalEndState(t *testing.T) {
	fixture := t.TempDir()
	buildPruneFixture(t, fixture)

	// --- Plain prune (no include_settings) ---
	dirOnline, dirOffline := t.TempDir(), t.TempDir()
	copyDir(t, fixture, dirOnline)
	copyDir(t, fixture, dirOffline)

	// Online: DELETE /all.
	sOnline, srv := newStoreWithServer(t, dirOnline)
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/all", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("online DELETE /all: status %d, want 200", resp.StatusCode)
	}
	if err := sOnline.Close(); err != nil {
		t.Fatal(err)
	}

	// Offline: PruneDataDir.
	if err := PruneDataDir(dirOffline, false); err != nil {
		t.Fatalf("offline PruneDataDir: %v", err)
	}

	// Expected end state: whatever the fixture had, minus agents + sessions
	// (cleared); rooms, messages, reactions, attachments are spared.
	before := reloadPruneCounts(t, fixture)
	wantPlain := before
	wantPlain.agents = 0
	wantPlain.sessions = 0
	if wantPlain.rooms == 0 || wantPlain.messages == 0 || wantPlain.reactions == 0 || wantPlain.attachments == 0 {
		t.Fatalf("fixture undercounted: %+v — test is meaningless without spared content", before)
	}

	online := reloadPruneCounts(t, dirOnline)
	offline := reloadPruneCounts(t, dirOffline)
	if online != wantPlain {
		t.Fatalf("online plain prune end state = %+v, want %+v", online, wantPlain)
	}
	if offline != wantPlain {
		t.Fatalf("offline plain prune end state = %+v, want %+v", offline, wantPlain)
	}
	if online != offline {
		t.Fatalf("PLAIN PRUNE DIVERGENCE: online %+v vs offline %+v — these must match (the 2026-08-07 loss was this kind of divergence)", online, offline)
	}

	// --- prune -a (include_settings=true) ---
	dirOnlineA, dirOfflineA := t.TempDir(), t.TempDir()
	copyDir(t, fixture, dirOnlineA)
	copyDir(t, fixture, dirOfflineA)

	sOnlineA, srvA := newStoreWithServer(t, dirOnlineA)
	reqA, _ := http.NewRequest(http.MethodDelete, srvA.URL+"/all?include_settings=true", nil)
	respA, err := http.DefaultClient.Do(reqA)
	if err != nil {
		t.Fatal(err)
	}
	respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("online DELETE /all?include_settings=true: status %d, want 200", respA.StatusCode)
	}
	if err := sOnlineA.Close(); err != nil {
		t.Fatal(err)
	}

	if err := PruneDataDir(dirOfflineA, true); err != nil {
		t.Fatalf("offline PruneDataDir -a: %v", err)
	}

	// -a wipes conversation + agent state. On reload ensureSystemRoom re-creates
	// the _system room, so rooms may be 1; messages, agents, sessions, reactions,
	// and attachments must all be 0.
	onlineA := reloadPruneCounts(t, dirOnlineA)
	offlineA := reloadPruneCounts(t, dirOfflineA)
	for _, c := range []struct {
		name string
		got  pruneCounts
	}{{"online", onlineA}, {"offline", offlineA}} {
		if c.got.messages != 0 || c.got.agents != 0 || c.got.sessions != 0 || c.got.reactions != 0 || c.got.attachments != 0 {
			t.Fatalf("%s prune -a end state = %+v, want only the _system room (everything else wiped)", c.name, c.got)
		}
		if c.got.rooms > 1 {
			t.Fatalf("%s prune -a rooms = %d, want <=1 (only _system re-created on reload)", c.name, c.got.rooms)
		}
	}
	if onlineA != offlineA {
		t.Fatalf("PRUNE -a DIVERGENCE: online %+v vs offline %+v — these must match", onlineA, offlineA)
	}
}

// TestPrunePlainFreesSingletonRolesFromGhosts reproduces the bug wren caught in
// item 4: clearAll(false) used to leave room.Roles referencing cleared agent
// IDs, and assignRole's singleton guard never checks the holder still exists —
// so a pruned room could never appoint a new leader (ghost holder), and
// restart didn't heal it (pruneOnStartup iterates the now-empty s.agents).
// Plain prune must drop cleared agents from membership AND roles.
func TestPrunePlainFreesSingletonRolesFromGhosts(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	oldbot, _, err := s.registerAI("gpt5", "codex", "test", nil, "oldbot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", oldbot.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.assignRole("general", oldbot.ID, "leader"); err != nil {
		t.Fatalf("assign leader to oldbot: %v", err)
	}

	// Plain prune clears all agents — and must clear their room roles too.
	s.clearAll(false)

	// Read state under the lock, then assert outside it: never hold a lock
	// across t.Fatal/t.Errorf — runtime.Goexit would skip the RUnlock and leave
	// the RWMutex read-locked for the rest of the process.
	s.mu.RLock()
	room := s.rooms["general"]
	members := append([]string(nil), room.Members...)
	roles := make(map[string]string, len(room.Roles))
	for k, v := range room.Roles {
		roles[k] = v
	}
	s.mu.RUnlock()

	// State checks use t.Errorf (not Fatalf) so a failing run reports all three:
	// the cause (ghost state) and the consequence (can't assign) together.
	if len(members) != 0 {
		t.Errorf("room.Members after plain prune = %v, want empty (cleared agents dropped)", members)
	}
	if len(roles) != 0 {
		t.Errorf("room.Roles after plain prune = %+v, want empty (ghost holder not cleared)", roles)
	}

	// Consequence assertion: a freshly registered agent can take the singleton
	// leader role. Guards a future break that leaves state clean but behaviour
	// broken — the state checks above don't catch that class on their own.
	newbot, _, err := s.registerAI("gpt5", "codex", "test", nil, "newbot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", newbot.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.assignRole("general", newbot.ID, "leader"); err != nil {
		t.Errorf("fresh agent could not take leader after plain prune: %v (ghost holder not cleared)", err)
	}
}

// TestPruneAllSparesSwitcherCredentials verifies that prune -a (the destructive
// mode) does not touch the switcher/ directory — the user's stored logins are
// not conversation state and no flag advertises destroying them. Losing
// switcher/ costs a browser re-login per account.
func TestPruneAllSparesSwitcherCredentials(t *testing.T) {
	dir := t.TempDir()

	// Seed a plausible switcher/ layout with fixture bytes (never real creds).
	switcherDir := filepath.Join(dir, "switcher")
	credDir := filepath.Join(switcherDir, "profiles", "claude", "main")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateJSON := `{"enabled":true,"active":{"claude":"main"}}`
	credJSON := `{"claudeAiOauth":{"accessToken":"fake-access","refreshToken":"fake-refresh","expiresAt":9999999999}}`
	if err := os.WriteFile(filepath.Join(switcherDir, "state.json"), []byte(stateJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, ".credentials.json"), []byte(credJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// Run prune -a (the destructive mode).
	if err := PruneDataDir(dir, true); err != nil {
		t.Fatalf("PruneDataDir(includeSettings=true) failed: %v", err)
	}

	// Assert both files survive with their original contents.
	gotState, err := os.ReadFile(filepath.Join(switcherDir, "state.json"))
	if err != nil {
		t.Fatalf("switcher/state.json missing after prune -a: %v (stored logins destroyed)", err)
	}
	if string(gotState) != stateJSON {
		t.Errorf("switcher/state.json contents changed after prune -a:\nwant: %s\ngot:  %s", stateJSON, gotState)
	}

	gotCred, err := os.ReadFile(filepath.Join(credDir, ".credentials.json"))
	if err != nil {
		t.Fatalf("switcher/profiles/claude/main/.credentials.json missing after prune -a: %v (stored login destroyed)", err)
	}
	if string(gotCred) != credJSON {
		t.Errorf("credentials file contents changed after prune -a:\nwant: %s\ngot:  %s", credJSON, gotCred)
	}
}
