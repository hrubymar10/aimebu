package switcher

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

// Every fixture here lives under t.TempDir(): the Manager is told its home explicitly and
// CLAUDE_CONFIG_DIR is cleared, and assertFixtureOnly proves the live path it will touch is inside
// the test's own tree and not under the real home, so a mistake in a helper fails the test instead
// of reaching ~/.claude.
func assertFixtureOnly(t *testing.T, m *Manager) string {
	t.Helper()
	if err := testenv.Check(); err != nil {
		t.Fatalf("test environment is not isolated: %v", err)
	}
	live, err := m.liveCredPath(ToolClaude)
	must(t, err)
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	must(t, err)
	dir := filepath.Dir(live)
	if resolved, evalErr := filepath.EvalSymlinks(filepath.Dir(dir)); evalErr == nil {
		dir = filepath.Join(resolved, filepath.Base(dir))
	}
	if !strings.HasPrefix(dir, tmp+string(filepath.Separator)) {
		t.Fatalf("live credential path %s is outside the temp tree %s", live, tmp)
	}
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		t.Fatalf("CLAUDE_CONFIG_DIR is set to %s", os.Getenv("CLAUDE_CONFIG_DIR"))
	}
	return live
}

// mcpEntry is one MCP server login as Claude writes it under mcpOAuth.
func mcpEntry(token string) string {
	return `{"serverName":"srv","accessToken":"` + token + `","expiresAt":1900000000000}`
}

// claudeDoc is a Claude credential document: the account login of futureClaude(tok) plus the given
// mcpOAuth members (key → entry), or no mcpOAuth section when servers is nil.
func claudeDoc(t *testing.T, tok string, servers map[string]string) string {
	t.Helper()
	var doc map[string]json.RawMessage
	must(t, json.Unmarshal([]byte(futureClaude(tok)), &doc))
	if servers != nil {
		parts := make([]string, 0, len(servers))
		for _, key := range sortedKeys(servers) {
			k, _ := json.Marshal(key)
			parts = append(parts, string(k)+":"+servers[key])
		}
		doc["mcpOAuth"] = json.RawMessage("{" + strings.Join(parts, ",") + "}")
	}
	out, err := json.Marshal(doc)
	must(t, err)
	return string(out)
}

// withMCPRaw appends an mcpOAuth member with the given raw JSON value to a one-object document.
func withMCPRaw(doc, raw string) string {
	return strings.TrimSuffix(doc, "}") + `,"mcpOAuth":` + raw + "}"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// mcpOf returns the mcpOAuth map of a credential document (nil when it has none).
func mcpOf(t *testing.T, doc string) map[string]json.RawMessage {
	t.Helper()
	var root map[string]json.RawMessage
	must(t, json.Unmarshal([]byte(doc), &root))
	raw, ok := root["mcpOAuth"]
	if !ok {
		return nil
	}
	var out map[string]json.RawMessage
	must(t, json.Unmarshal(raw, &out))
	return out
}

// linkPair builds two Claude profiles in link mode: a (active, live holds liveDoc) and b (stored bDoc).
func linkPair(t *testing.T, liveDoc, bDoc string) (m *Manager, live string) {
	t.Helper()
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	live = writeLive(t, home, ToolClaude, liveDoc)
	assertFixtureOnly(t, m)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(bDoc), 0o600))
	return m, live
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestLinkSwitchCarriesMCPLoginsAndKeepsTheAccountsApart(t *testing.T) {
	m, live := linkPair(t,
		claudeDoc(t, "a", map[string]string{"s1|x": mcpEntry("mcp-live-1")}),
		claudeDoc(t, "b", map[string]string{"s2|y": mcpEntry("mcp-b-2")}))

	active, switched, err := m.Switch(ToolClaude, "b")
	if err != nil || !switched || active != "b" {
		t.Fatalf("Switch = %q %v %v", active, switched, err)
	}
	got := readFile(t, live)
	servers := mcpOf(t, got)
	if len(servers) != 2 || !strings.Contains(string(servers["s1|x"]), "mcp-live-1") || !strings.Contains(string(servers["s2|y"]), "mcp-b-2") {
		t.Fatalf("live mcpOAuth after the switch = %s", got)
	}
	// The account is b's: nothing of a's login crossed over.
	if !strings.Contains(got, `"accessToken":"b"`) || !strings.Contains(got, `"refreshToken":"refresh-b"`) || strings.Contains(got, "refresh-a") {
		t.Fatalf("live account login after the switch = %s", got)
	}
	// The outgoing profile was captured whole, MCP logins included, as before.
	if stored := readFile(t, m.profileCredPath(ToolClaude, "a")); !strings.Contains(stored, "refresh-a") || !strings.Contains(stored, "mcp-live-1") {
		t.Fatalf("outgoing capture = %s", stored)
	}
	// The next switch carries the merged map back the other way.
	if _, _, err := m.Switch(ToolClaude, "a"); err != nil {
		t.Fatal(err)
	}
	back := readFile(t, live)
	if len(mcpOf(t, back)) != 2 || !strings.Contains(back, "refresh-a") || strings.Contains(back, "refresh-b") {
		t.Fatalf("live after switching back = %s", back)
	}
}

func TestLinkSwitchConflictKeepsTheActiveLoginAndBacksUpTheDestination(t *testing.T) {
	logs := captureLog(t)
	bDoc := claudeDoc(t, "b", map[string]string{"s1|x": mcpEntry("mcp-secret-old"), "s3|z": mcpEntry("mcp-secret-b3")})
	m, live := linkPair(t, claudeDoc(t, "a", map[string]string{"s1|x": mcpEntry("mcp-secret-new")}), bDoc)

	if _, _, err := m.Switch(ToolClaude, "b"); err != nil {
		t.Fatal(err)
	}
	servers := mcpOf(t, readFile(t, live))
	if !strings.Contains(string(servers["s1|x"]), "mcp-secret-new") || strings.Contains(string(servers["s1|x"]), "mcp-secret-old") {
		t.Fatalf("the active login did not win: %s", servers["s1|x"])
	}
	if !strings.Contains(string(servers["s3|z"]), "mcp-secret-b3") {
		t.Fatalf("a server only the destination had was dropped: %v", servers)
	}
	// The destination's own copy is in its backups, byte for byte, 0600.
	var found bool
	entries, err := os.ReadDir(filepath.Join(m.backupsRoot(), ToolClaude))
	must(t, err)
	for _, e := range entries {
		path := filepath.Join(m.backupsRoot(), ToolClaude, e.Name())
		if strings.HasPrefix(e.Name(), "b-") && readFile(t, path) == bDoc {
			found = true
			if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
				t.Fatalf("backup mode = %v", info.Mode().Perm())
			}
		}
	}
	if !found {
		t.Fatalf("no backup of the destination's pre-switch bytes in %v", entries)
	}
	// Names only in the log: the server key and the profile, never a token.
	out := logs.String()
	if !strings.Contains(out, "s1|x") || !strings.Contains(out, "claude/b") {
		t.Fatalf("conflict not reported by key and profile: %q", out)
	}
	for _, secret := range []string{"mcp-secret-new", "mcp-secret-old", "mcp-secret-b3", "refresh-a", "refresh-b"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaks %q: %s", secret, out)
		}
	}
}

func TestLinkSwitchWithoutMCPLoginsChangesNothingAboutTheRestoredBytes(t *testing.T) {
	bDoc := claudeDoc(t, "b", nil)
	m, live := linkPair(t, claudeDoc(t, "a", nil), bDoc)
	if _, _, err := m.Switch(ToolClaude, "b"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, live); got != bDoc {
		t.Fatalf("live = %s want the stored bytes untouched", got)
	}
	// Only the incoming side has MCP logins: they stay.
	bWith := claudeDoc(t, "b", map[string]string{"s2|y": mcpEntry("mcp-b")})
	m2, live2 := linkPair(t, claudeDoc(t, "a", nil), bWith)
	if _, _, err := m2.Switch(ToolClaude, "b"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, live2); got != bWith {
		t.Fatalf("live = %s want %s", got, bWith)
	}
}

func TestLinkSwitchWithUnreadableMCPSectionsStillSwitches(t *testing.T) {
	t.Run("live section malformed", func(t *testing.T) {
		logs := captureLog(t)
		bDoc := claudeDoc(t, "b", map[string]string{"s2|y": mcpEntry("mcp-b")})
		liveDoc := withMCPRaw(futureClaude("a"), `"oops-not-an-object"`)
		m, live := linkPair(t, liveDoc, bDoc)
		if _, _, err := m.Switch(ToolClaude, "b"); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, live); got != bDoc {
			t.Fatalf("live = %s want the stored bytes", got)
		}
		if !strings.Contains(logs.String(), "MCP logins not carried over") {
			t.Fatalf("skip not logged: %q", logs.String())
		}
		// The live file was backed up as always, so nothing is lost.
		var sawBackup bool
		entries, _ := os.ReadDir(filepath.Join(m.backupsRoot(), ToolClaude))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "a-") && strings.Contains(readFile(t, filepath.Join(m.backupsRoot(), ToolClaude, e.Name())), "oops-not-an-object") {
				sawBackup = true
			}
		}
		if !sawBackup {
			t.Fatal("the unreadable live file was not backed up")
		}
	})
	t.Run("destination section malformed", func(t *testing.T) {
		logs := captureLog(t)
		bDoc := withMCPRaw(futureClaude("b"), `["broken"]`)
		m, live := linkPair(t, claudeDoc(t, "a", map[string]string{"s1|x": mcpEntry("mcp-live")}), bDoc)
		if _, _, err := m.Switch(ToolClaude, "b"); err != nil {
			t.Fatal(err)
		}
		if servers := mcpOf(t, readFile(t, live)); len(servers) != 1 || !strings.Contains(string(servers["s1|x"]), "mcp-live") {
			t.Fatalf("live = %s", readFile(t, live))
		}
		if !strings.Contains(logs.String(), "unreadable") {
			t.Fatalf("replacement not logged: %q", logs.String())
		}
		var backedUp bool
		entries, _ := os.ReadDir(filepath.Join(m.backupsRoot(), ToolClaude))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "b-") && readFile(t, filepath.Join(m.backupsRoot(), ToolClaude, e.Name())) == bDoc {
				backedUp = true
			}
		}
		if !backedUp {
			t.Fatal("the destination's unreadable copy was not backed up first")
		}
	})
}

func TestLinkSwitchPicksUpAnMCPLoginRotatedJustBeforeTheLock(t *testing.T) {
	m, live := linkPair(t,
		claudeDoc(t, "a", map[string]string{"s1|x": mcpEntry("mcp-before")}),
		claudeDoc(t, "b", nil))
	// The CLI refreshes the MCP token after the caller decided to switch but before the switcher holds
	// its locks: the merge must read the file as it is then.
	m.beforeSwitchLock = func() {
		must(t, os.WriteFile(live, []byte(claudeDoc(t, "a", map[string]string{"s1|x": mcpEntry("mcp-rotated")})), 0o600))
	}
	if _, _, err := m.Switch(ToolClaude, "b"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, live)
	if !strings.Contains(got, "mcp-rotated") || strings.Contains(got, "mcp-before") {
		t.Fatalf("live = %s", got)
	}
}

func TestLinkSwitchIsRefusedWhileClaudeHoldsTheStorageLock(t *testing.T) {
	old := storageLockTimeout
	storageLockTimeout = 60 * time.Millisecond
	t.Cleanup(func() { storageLockTimeout = old })
	liveDoc := claudeDoc(t, "a", map[string]string{"s1|x": mcpEntry("mcp-live")})
	m, live := linkPair(t, liveDoc, claudeDoc(t, "b", nil))
	// A lock directory the CLI is holding and keeping fresh.
	lockDir := credtarget.StorageWriteLockPath(filepath.Dir(live))
	must(t, os.Mkdir(lockDir, 0o700))

	_, switched, err := m.Switch(ToolClaude, "b")
	if err == nil || switched || !errors.Is(err, credtarget.ErrLockBusy) {
		t.Fatalf("Switch = %v %v, want a refusal wrapping ErrLockBusy", switched, err)
	}
	if got := readFile(t, live); got != liveDoc {
		t.Fatalf("a refused switch changed the live file: %s", got)
	}
	if a, _ := m.Active(ToolClaude); a != "a" {
		t.Fatalf("active = %q after a refused switch", a)
	}
	if _, statErr := os.Stat(lockDir); statErr != nil {
		t.Fatal("the switch removed a lock it does not own")
	}
	// Released: the switch goes through, and leaves no lock behind.
	must(t, os.Remove(lockDir))
	if _, _, err := m.Switch(ToolClaude, "b"); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(lockDir); !os.IsNotExist(statErr) {
		t.Fatalf("storage lock left behind: %v", statErr)
	}
}

func TestLinkSwitchRollsBackTheMergedLiveFileWhenTheStateSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; the state write cannot be made to fail")
	}
	liveDoc := claudeDoc(t, "a", map[string]string{"s1|x": mcpEntry("mcp-live")})
	m, live := linkPair(t, liveDoc, claudeDoc(t, "b", nil))
	// Everything a switch writes before the state file goes to directories under the root that already
	// exist; only state.json (replaced in the root itself) fails once the root is read-only.
	must(t, os.MkdirAll(filepath.Join(m.root, ".maintenance"), 0o700))
	f, err := os.OpenFile(filepath.Join(m.root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	must(t, err)
	must(t, f.Close())
	must(t, os.Chmod(m.root, 0o500))
	t.Cleanup(func() { _ = os.Chmod(m.root, 0o700) })

	_, switched, err := m.Switch(ToolClaude, "b")
	if err == nil || switched || !strings.Contains(err.Error(), "update state") {
		t.Fatalf("Switch should fail at the state write, got %v %v", switched, err)
	}
	if got := readFile(t, live); got != liveDoc {
		t.Fatalf("live not rolled back: %s", got)
	}
	must(t, os.Chmod(m.root, 0o700))
	if a, _ := m.Active(ToolClaude); a != "a" {
		t.Fatalf("active = %q after a failed switch", a)
	}
	if info, statErr := os.Lstat(m.profileCredPath(ToolClaude, "a")); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the outgoing profile was not re-linked to live: %v", statErr)
	}
}

func TestLinkSwitchToAnEmptyProfileKeepsMCPLoginsOnly(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	live := writeLive(t, home, ToolClaude, claudeDoc(t, "a", map[string]string{"s1|x": mcpEntry("mcp-live")}))
	assertFixtureOnly(t, m)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "empty"))

	if _, switched, err := m.Switch(ToolClaude, "empty"); err != nil || !switched {
		t.Fatalf("Switch = %v %v", switched, err)
	}
	got := readFile(t, live)
	if strings.Contains(got, "claudeAiOauth") || strings.Contains(got, "refresh-a") {
		t.Fatalf("the outgoing account's login is still live: %s", got)
	}
	if servers := mcpOf(t, got); len(servers) != 1 || !strings.Contains(string(servers["s1|x"]), "mcp-live") {
		t.Fatalf("MCP login not kept: %s", got)
	}
	if info, _ := os.Stat(live); info.Mode().Perm() != 0o600 {
		t.Fatalf("live mode = %v", info.Mode().Perm())
	}
	// A file holding only MCP logins reads as logged out, not corrupt, so the user can keep switching,
	// and the MCP logins ride along to the next account.
	if el, err := m.eligibilityFor(ToolClaude); err != nil || !el.Absent || el.Eligible {
		t.Fatalf("eligibility = %+v %v, want absent", el, err)
	}
	if _, switched, err := m.Switch(ToolClaude, "a"); err != nil || !switched {
		t.Fatalf("switch back = %v %v", switched, err)
	}
	if back := readFile(t, live); !strings.Contains(back, "refresh-a") || !strings.Contains(string(mcpOf(t, back)["s1|x"]), "mcp-live") {
		t.Fatalf("live after switching back = %s", back)
	}
}

func TestLinkSwitchToAnEmptyProfileWithoutMCPLoginsRemovesTheFileAsBefore(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	live := writeLive(t, home, ToolClaude, claudeDoc(t, "a", nil))
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "empty"))
	if _, _, err := m.Switch(ToolClaude, "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("live file survived: %v", err)
	}
}

func TestLoggedOutPlaceholderWithMCPLoginsIsCarriedAndKeptOnRollback(t *testing.T) {
	// The live file is a logout placeholder (no login) that still holds MCP logins.
	placeholder := `{"claudeAiOauth":{"accessToken":"","refreshToken":""},"mcpOAuth":{"s1|x":` + mcpEntry("mcp-live") + `}}`
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	live := writeLive(t, home, ToolClaude, futureClaude("a"))
	assertFixtureOnly(t, m)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(claudeDoc(t, "b", nil)), 0o600))
	must(t, os.Remove(m.profileCredPath(ToolClaude, "a"))) // the link would dangle once live is replaced
	must(t, os.WriteFile(live, []byte(placeholder), 0o600))

	if _, switched, err := m.Switch(ToolClaude, "b"); err != nil || !switched {
		t.Fatalf("Switch = %v %v", switched, err)
	}
	got := readFile(t, live)
	if !strings.Contains(got, "refresh-b") || !strings.Contains(string(mcpOf(t, got)["s1|x"]), "mcp-live") {
		t.Fatalf("live = %s", got)
	}
}

// ── token-copy (migrated) installs ───────────────────────────────────────────

// tokenCopyReal builds a migrated install over the real fan-out: a active (live a token-only copy
// with MCP logins), b stored whole.
func tokenCopyReal(t *testing.T, liveMCP string) (m *Manager, live string) {
	t.Helper()
	m, home := newRealManager(t)
	must(t, m.SetEnabled(true))
	expiry := time.Now().Add(6 * time.Hour)
	live = writeLive(t, home, ToolClaude, fullClaude("tok-a", "ref-a", expiry))
	assertFixtureOnly(t, m)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(fullClaude("tok-b", "ref-b", expiry)), 0o600))
	must(t, os.Remove(m.profileCredPath(ToolClaude, "a")))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "a"), []byte(fullClaude("tok-a", "ref-a", expiry)), 0o600))
	tokenOnly := tokenOnlyClaude("tok-a", expiry)
	if liveMCP != "" {
		tokenOnly = withMCPRaw(tokenOnly, liveMCP)
	}
	must(t, os.WriteFile(live, []byte(tokenOnly), 0o600))
	must(t, m.SetTokenCopies(true))
	return m, live
}

func TestTokenCopySwitchKeepsTheLiveMCPLogins(t *testing.T) {
	m, live := tokenCopyReal(t, `{"s1|x":`+mcpEntry("mcp-live")+`}`)
	if _, switched, err := m.Switch(ToolClaude, "b"); err != nil || !switched {
		t.Fatalf("Switch = %v %v", switched, err)
	}
	got := readFile(t, live)
	if !strings.Contains(got, "tok-b") || strings.Contains(got, "tok-a") || strings.Contains(got, "refreshToken") {
		t.Fatalf("live account = %s", got)
	}
	if !strings.Contains(string(mcpOf(t, got)["s1|x"]), "mcp-live") {
		t.Fatalf("MCP login lost: %s", got)
	}
}

func TestTokenCopySwitchToAnEmptyProfileKeepsMCPLoginsOnly(t *testing.T) {
	m, live := tokenCopyReal(t, `{"s1|x":`+mcpEntry("mcp-live")+`}`)
	must(t, m.Add(ToolClaude, "empty"))
	if _, switched, err := m.Switch(ToolClaude, "empty"); err != nil || !switched {
		t.Fatalf("Switch = %v %v", switched, err)
	}
	got := readFile(t, live)
	if strings.Contains(got, "claudeAiOauth") || strings.Contains(got, "tok-a") {
		t.Fatalf("the outgoing account's token is still live: %s", got)
	}
	if !strings.Contains(string(mcpOf(t, got)["s1|x"]), "mcp-live") {
		t.Fatalf("MCP login lost: %s", got)
	}
	// And the install keeps working: the reduced file reads as logged out, switching to b rewrites the login.
	if _, switched, err := m.Switch(ToolClaude, "b"); err != nil || !switched {
		t.Fatalf("switch on = %v %v", switched, err)
	}
	if back := readFile(t, live); !strings.Contains(back, "tok-b") || !strings.Contains(back, "mcp-live") {
		t.Fatalf("live = %s", back)
	}
}

func TestTokenCopySwitchToAnEmptyProfileWithoutMCPLoginsRemovesTheFile(t *testing.T) {
	m, live := tokenCopyReal(t, "")
	must(t, m.Add(ToolClaude, "empty"))
	if _, _, err := m.Switch(ToolClaude, "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("live file survived: %v", err)
	}
}

func TestEmptyLiveLoginAcceptsMCPLoginsButNotJunk(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		doc  string
		want bool
	}{
		"mcp only":              {`{"mcpOAuth":{"s|1":{"accessToken":"x"}}}`, true},
		"mcp null":              {`{"mcpOAuth":null}`, true},
		"logged out with mcp":   {`{"claudeAiOauth":{"accessToken":"","refreshToken":""},"mcpOAuth":{"s|1":{}}}`, true},
		"mcp not an object":     {`{"mcpOAuth":"x"}`, false},
		"mcp with a live login": {`{"claudeAiOauth":{"accessToken":"live"},"mcpOAuth":{}}`, false},
		"mcp with other key":    {`{"mcpOAuth":{},"somethingElse":1}`, false},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		must(t, os.WriteFile(path, []byte(tc.doc), 0o600))
		if got := emptyLiveLogin(path, ToolClaude); got != tc.want {
			t.Errorf("%s: emptyLiveLogin = %v, want %v", name, got, tc.want)
		}
	}
}
