package switcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProfileNameChangesPreserveActiveLogin(t *testing.T) {
	for _, tool := range Tools() {
		for _, copies := range []bool{false, true} {
			model := "link"
			if copies {
				model = "copies"
			}
			t.Run(tool+"/"+model, func(t *testing.T) {
				m, home := newTestManager(t)
				must(t, m.SetEnabled(true))
				full := futureClaude("tok_aaaa")
				if tool == ToolCodex {
					full = futureCodex("user_test")
				}
				live := writeLive(t, home, tool, full)
				must(t, m.Import(tool, "main"))
				must(t, m.Add(tool, "spare"))
				if copies {
					stored := m.profileCredPath(tool, "main")
					must(t, os.Remove(stored))
					must(t, os.WriteFile(stored, []byte(full), 0o600))
					if tool == ToolClaude {
						must(t, os.WriteFile(live, []byte(tokenOnlyClaude("tok_aaaa", time.Now().Add(time.Hour))), 0o600))
					} else {
						must(t, os.WriteFile(live, []byte(`{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"tok_aaaa","refresh_token":""}}`), 0o600))
					}
					must(t, m.SetTokenCopies(true))
				}
				liveBefore := readFile(t, live)
				storedBefore := readFile(t, m.profileCredPath(tool, "main"))
				check := func(want string) {
					t.Helper()
					active, err := m.Active(tool)
					must(t, err)
					if active != want {
						t.Fatalf("active = %q, want %q", active, want)
					}
					if got := readFile(t, live); got != liveBefore {
						t.Fatal("live login changed")
					}
					if got := readFile(t, m.profileCredPath(tool, want)); got != storedBefore {
						t.Fatal("stored login changed")
					}
					restarted, err := New(m.root)
					must(t, err)
					choice, err := restarted.ManualChoice(tool)
					must(t, err)
					if choice.Profile != want || choice.At.IsZero() {
						t.Fatalf("manual choice after restart = %+v, want %s", choice, want)
					}
				}
				must(t, m.Rename(tool, "spare", "other"))
				check("main")
				must(t, m.Remove(tool, "other", false))
				check("main")
				must(t, m.Rename(tool, "main", "primary"))
				check("primary")
			})
		}
	}
}

func TestEmptyProfileRemovalAndRenameCollision(t *testing.T) {
	for _, copies := range []bool{false, true} {
		model := "link"
		if copies {
			model = "copies"
		}
		t.Run(model, func(t *testing.T) {
			m, home := newTestManager(t)
			must(t, m.SetEnabled(true))
			full := futureCodex("user_test")
			live := writeLive(t, home, ToolCodex, full)
			must(t, m.Import(ToolCodex, "main"))
			if copies {
				stored := m.profileCredPath(ToolCodex, "main")
				must(t, os.Remove(stored))
				must(t, os.WriteFile(stored, []byte(full), 0o600))
				must(t, os.WriteFile(live, []byte(`{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"tok_aaaa","refresh_token":""}}`), 0o600))
				must(t, m.SetTokenCopies(true))
			}
			before := readFile(t, live)
			must(t, m.Add(ToolCodex, "empty"))
			if err := m.Rename(ToolCodex, "main", "empty"); !errors.Is(err, ErrProfileExists) {
				t.Fatalf("rename onto empty profile = %v", err)
			}
			must(t, m.Add(ToolCodex, "occupied"))
			must(t, os.WriteFile(m.profileCredPath(ToolCodex, "occupied"), []byte(futureCodex("user_other")), 0o600))
			if err := m.Rename(ToolCodex, "main", "occupied"); !errors.Is(err, ErrProfileExists) {
				t.Fatalf("rename onto occupied profile = %v", err)
			}
			if active, err := m.Active(ToolCodex); err != nil || active != "main" {
				t.Fatalf("active after refused renames = %q, %v", active, err)
			}
			if got := readFile(t, live); got != before {
				t.Fatal("refused rename changed live login")
			}
			must(t, m.Remove(ToolCodex, "empty", false))
			if _, err := os.Stat(m.profileDir(ToolCodex, "empty")); !os.IsNotExist(err) {
				t.Fatalf("empty profile remains: %v", err)
			}
			if active, err := m.Active(ToolCodex); err != nil || active != "main" {
				t.Fatalf("active after empty removal = %q, %v", active, err)
			}
			if got := readFile(t, live); got != before {
				t.Fatal("empty profile removal changed live login")
			}
		})
	}
}

func TestRemoveActiveEmptyProfile(t *testing.T) {
	for _, copies := range []bool{false, true} {
		model := "link"
		if copies {
			model = "copies"
		}
		t.Run(model, func(t *testing.T) {
			m, home := newTestManager(t)
			must(t, m.SetEnabled(true))
			must(t, m.Add(ToolCodex, "empty"))
			state, err := m.loadState()
			must(t, err)
			state.Active[ToolCodex] = "empty"
			must(t, m.saveState(state))
			if copies {
				must(t, m.SetTokenCopies(true))
			}
			if err := m.Remove(ToolCodex, "empty", false); !errors.Is(err, ErrActiveProfile) {
				t.Fatalf("unconfirmed active removal = %v", err)
			}
			must(t, m.Remove(ToolCodex, "empty", true))
			if active, err := m.Active(ToolCodex); err != nil || active != "" {
				t.Fatalf("active after removal = %q, %v", active, err)
			}
			if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); !os.IsNotExist(err) {
				t.Fatalf("live login after empty removal: %v", err)
			}
		})
	}
}

func TestRenameAfterImportsFromSeparateCodexHomes(t *testing.T) {
	for _, copies := range []bool{false, true} {
		model := "link"
		if copies {
			model = "copies"
		}
		t.Run(model, func(t *testing.T) {
			m, home := newTestManager(t)
			t.Setenv("HOME", home)
			m.homeDir = ""
			must(t, m.SetEnabled(true))
			if copies {
				must(t, m.SetTokenCopies(true))
				m.SetTokenCopyHooks(TokenCopyHooks{ReplaceActive: func(tool string, _ []byte) error {
					path, err := m.liveCredPath(tool)
					if err != nil {
						return err
					}
					return os.WriteFile(path, []byte(`{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"tok_aaaa","refresh_token":""}}`), 0o600)
				}})
			}
			for _, name := range []string{"bkp", "bkp2", "bkp3"} {
				must(t, m.Add(ToolCodex, name))
			}
			defaultLive := writeLive(t, home, ToolCodex, futureCodex("user_default"))
			defaultBefore := readFile(t, defaultLive)
			var thirdLive string
			for i, name := range []string{"bkp-t1", "bkp-t2", "bkp-t3"} {
				alt := filepath.Join(home, "alt", name)
				t.Setenv("CODEX_HOME", alt)
				must(t, os.MkdirAll(alt, 0o700))
				path := filepath.Join(alt, "auth.json")
				must(t, os.WriteFile(path, []byte(futureCodex("user_test_"+string(rune('1'+i)))), 0o600))
				must(t, m.Import(ToolCodex, name))
				if i == 2 {
					thirdLive = path
				}
			}
			t.Setenv("CODEX_HOME", "")
			thirdBefore := readFile(t, thirdLive)
			for _, name := range []string{"bkp", "bkp2", "bkp3"} {
				must(t, m.Remove(ToolCodex, name, false))
			}
			for i, name := range []string{"bkp", "bkp2", "bkp3"} {
				must(t, m.Rename(ToolCodex, []string{"bkp-t1", "bkp-t2", "bkp-t3"}[i], name))
			}
			if active, err := m.Active(ToolCodex); err != nil || active != "bkp3" {
				t.Fatalf("active after sequence = %q, %v", active, err)
			}
			if got := readFile(t, defaultLive); got != defaultBefore {
				t.Fatal("default live login changed")
			}
			if got := readFile(t, thirdLive); got != thirdBefore {
				t.Fatal("third imported live login changed")
			}
			for _, name := range []string{"bkp", "bkp2", "bkp3"} {
				if _, err := os.ReadFile(m.profileCredPath(ToolCodex, name)); err != nil {
					t.Fatalf("stored profile %s: %v", name, err)
				}
			}
		})
	}
}
