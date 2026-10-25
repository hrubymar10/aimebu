package credtarget

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	acct  = `"claudeAiOauth":{"accessToken":"acc","refreshToken":"ref","expiresAt":1}`
	acctB = `"claudeAiOauth":{"accessToken":"acc-b","refreshToken":"ref-b","expiresAt":2}`
)

func TestMergeClaudeMCPOAuth(t *testing.T) {
	t.Run("union, live wins a conflict, the account stays the incoming one", func(t *testing.T) {
		incoming := `{` + acctB + `,"other":1,"mcpOAuth":{"s2":{"t":"b2"},"s1":{"t":"old"}}}`
		live := `{` + acct + `,"mcpOAuth":{"s1":{"t":"new"},"s3":{"t":"l3"}}}`
		res, err := MergeClaudeMCPOAuth([]byte(incoming), []byte(live))
		if err != nil {
			t.Fatal(err)
		}
		want := `{` + acctB + `,"other":1,"mcpOAuth":{"s2":{"t":"b2"},"s1":{"t":"new"},"s3":{"t":"l3"}}}`
		if string(res.Merged) != want {
			t.Fatalf("merged = %s\nwant     %s", res.Merged, want)
		}
		if !reflect.DeepEqual(res.Conflicts, []string{"s1"}) || !res.LosesIncoming() || res.Replaced {
			t.Fatalf("result = %+v", res)
		}
		if strings.Contains(string(res.Merged), `"ref"`) {
			t.Fatal("the live account's refresh token crossed over")
		}
	})
	t.Run("same entry spelled with other whitespace is no conflict", func(t *testing.T) {
		res, err := MergeClaudeMCPOAuth(
			[]byte(`{"mcpOAuth":{"s1":{"t": "x"}}}`),
			[]byte(`{"mcpOAuth":{"s1":{"t":"x"}}}`))
		if err != nil || len(res.Conflicts) != 0 || res.LosesIncoming() {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("nothing to carry returns the incoming bytes", func(t *testing.T) {
		incoming := []byte(`{` + acctB + `}`)
		for name, live := range map[string]string{
			"no section": `{` + acct + `}`,
			"null":       `{"mcpOAuth":null}`,
			"empty":      `{"mcpOAuth":{}}`,
			"empty file": ``,
		} {
			res, err := MergeClaudeMCPOAuth(incoming, []byte(live))
			if err != nil || string(res.Merged) != string(incoming) {
				t.Errorf("%s: %s %v", name, res.Merged, err)
			}
		}
	})
	t.Run("incoming without a section takes the live one whole", func(t *testing.T) {
		res, err := MergeClaudeMCPOAuth([]byte(`{`+acctB+`}`), []byte(`{`+acct+`,"mcpOAuth":{"s1":{"t":"l"}}}`))
		if err != nil || string(res.Merged) != `{`+acctB+`,"mcpOAuth":{"s1":{"t":"l"}}}` || res.LosesIncoming() {
			t.Fatalf("%s %+v %v", res.Merged, res, err)
		}
	})
	t.Run("unreadable live section is an error", func(t *testing.T) {
		for _, live := range []string{`{"mcpOAuth":"x"}`, `{"mcpOAuth":[1]}`, `{"mcpOAuth":`, `[]`} {
			if _, err := MergeClaudeMCPOAuth([]byte(`{`+acctB+`}`), []byte(live)); err == nil {
				t.Errorf("live %s: no error", live)
			}
		}
	})
	t.Run("unreadable incoming section is replaced and flagged", func(t *testing.T) {
		res, err := MergeClaudeMCPOAuth([]byte(`{`+acctB+`,"mcpOAuth":"x"}`), []byte(`{"mcpOAuth":{"s1":{"t":"l"}}}`))
		if err != nil || !res.Replaced || !res.LosesIncoming() || !strings.Contains(string(res.Merged), `"mcpOAuth":{"s1":{"t":"l"}}`) {
			t.Fatalf("%s %+v %v", res.Merged, res, err)
		}
	})
}

func TestClaudeMCPOnly(t *testing.T) {
	only, ok := ClaudeMCPOnly([]byte(`{` + acct + `,"other":1,"mcpOAuth":{"s1":{"t":"x"}}}`))
	if !ok || string(only) != `{"mcpOAuth":{"s1":{"t":"x"}}}`+"\n" {
		t.Fatalf("%q %v", only, ok)
	}
	if again, ok := ClaudeMCPOnly(only); !ok || string(again) != string(only) {
		t.Fatalf("not idempotent: %q", again)
	}
	for _, doc := range []string{`{` + acct + `}`, `{"mcpOAuth":{}}`, `{"mcpOAuth":null}`, `{"mcpOAuth":"x"}`, `junk`, ``} {
		if got, ok := ClaudeMCPOnly([]byte(doc)); ok {
			t.Errorf("%q kept %q", doc, got)
		}
	}
}

func mcpClaudeTarget(t *testing.T, content string) Target {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".credentials.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Target{ID: "live", Tool: ToolClaude, Path: path, Role: RoleLive}
}

func TestClearTargetKeepsMCPLoginsAndRefusesAnUnimportedLogin(t *testing.T) {
	w := &Writer{}
	// A token-only copy with MCP logins keeps them.
	tgt := mcpClaudeTarget(t, `{"claudeAiOauth":{"accessToken":"acc","expiresAt":1},"mcpOAuth":{"s1":{"t":"x"}}}`)
	if err := w.ClearTarget(tgt); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(tgt.Path)
	if string(got) != `{"mcpOAuth":{"s1":{"t":"x"}}}`+"\n" {
		t.Fatalf("after clear = %s", got)
	}
	if info, _ := os.Stat(tgt.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if _, err := os.Stat(StorageWriteLockPath(filepath.Dir(tgt.Path))); !os.IsNotExist(err) {
		t.Fatalf("storage lock left behind: %v", err)
	}
	// Clearing again changes nothing.
	if err := w.ClearTarget(tgt); err != nil {
		t.Fatal(err)
	}
	// No MCP logins: the file goes, as it always did.
	bare := mcpClaudeTarget(t, `{"claudeAiOauth":{"accessToken":"acc","expiresAt":1}}`)
	if err := w.ClearTarget(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bare.Path); !os.IsNotExist(err) {
		t.Fatalf("file survived: %v", err)
	}
	// A target that is already gone counts as cleared.
	if err := w.ClearTarget(bare); err != nil {
		t.Fatal(err)
	}
	// A login nobody imported is never destroyed, MCP logins or not.
	login := `{"claudeAiOauth":{"accessToken":"acc","refreshToken":"ref","expiresAt":1},"mcpOAuth":{"s1":{"t":"x"}}}`
	held := mcpClaudeTarget(t, login)
	if err := w.ClearTarget(held); !errors.Is(err, ErrRefreshTokenPresent) {
		t.Fatalf("err = %v, want ErrRefreshTokenPresent", err)
	}
	if got, _ := os.ReadFile(held.Path); string(got) != login {
		t.Fatalf("the unimported login was changed: %s", got)
	}
	// Other tools' files are removed.
	codex := Target{ID: "codex", Tool: ToolCodex, Path: filepath.Join(t.TempDir(), "auth.json"), Role: RoleLive}
	if err := os.WriteFile(codex.Path, []byte(`{"tokens":{"access_token":"x","refresh_token":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.ClearTarget(codex); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(codex.Path); !os.IsNotExist(err) {
		t.Fatalf("codex file survived: %v", err)
	}
}

func TestClearTargetWaitsForTheStorageLock(t *testing.T) {
	tgt := mcpClaudeTarget(t, `{"claudeAiOauth":{"accessToken":"acc","expiresAt":1},"mcpOAuth":{"s1":{"t":"x"}}}`)
	if err := os.Mkdir(StorageWriteLockPath(filepath.Dir(tgt.Path)), 0o700); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(tgt.Path)
	err := (&Writer{LockTimeout: 50 * time.Millisecond}).ClearTarget(tgt)
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("err = %v, want ErrLockBusy", err)
	}
	if after, _ := os.ReadFile(tgt.Path); string(after) != string(before) {
		t.Fatalf("changed under a held lock: %s", after)
	}
}

func TestLockStorageIsExclusiveAndReleasable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w := &Writer{LockTimeout: 50 * time.Millisecond}
	release, err := w.LockStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.LockStorage(dir); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("second lock err = %v", err)
	}
	if lost := release(); lost {
		t.Fatal("heartbeat reported lost")
	}
	release2, err := w.LockStorage(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release2()
}
