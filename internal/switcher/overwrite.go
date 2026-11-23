package switcher

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/credtarget"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

// Installed is what a switch put into the live credential file: the profile switched to, the bytes the switch wrote (taken from the stored
// profile, never read back from the live file) and, for Codex, the account they name. Data is nil when the profile switched to had no stored
// credentials (the live login was cleared).
type Installed struct {
	Profile string
	Account string
	Data    []byte
}

func (m *Manager) recordInstalled(tool, profile string, data []byte) {
	in := Installed{Profile: profile, Data: data}
	if tool == ToolCodex && data != nil {
		in.Account = credtarget.CodexAccount(data)
	}
	m.installedMu.Lock()
	defer m.installedMu.Unlock()
	if m.installed == nil {
		m.installed = map[string]Installed{}
	}
	m.installed[tool] = in
}

// LastInstalled is what the last switch of tool made through this Manager installed. The Codex overwrite check takes the account it expects from
// here: re-reading the live file after the switch could already find the old account, written back by a codex process that was running.
func (m *Manager) LastInstalled(tool string) (Installed, bool) {
	m.installedMu.Lock()
	defer m.installedMu.Unlock()
	in, ok := m.installed[tool]
	return in, ok
}

// LiveAccount reads the live credential file of tool and the account it names ("" when it names none, or for a tool other than Codex). It takes no
// lock: it is the cheap read of the Codex overwrite check, which compares the account only and repairs through ReinstallIfOverwritten.
func (m *Manager) LiveAccount(tool string) (data []byte, account string, err error) {
	path, err := m.LiveCredentialPath(tool)
	if err != nil {
		return nil, "", err
	}
	if data, err = os.ReadFile(path); err != nil {
		return nil, "", err
	}
	if tool == ToolCodex {
		account = credtarget.CodexAccount(data)
	}
	return data, account, nil
}

// overwriteAccountWait bounds how long the overwrite check waits for the account lock of the profile whose tokens it saves; a busy account
// (a refresh or warm-up of it) is left for the next check.
var overwriteAccountWait = 2 * time.Second

// ReinstallIfOverwritten repairs the live login of a Codex switch when the live file names, instead of the account the switch installed, the
// account of another of the tool's stored profiles: a codex process that was running at the switch can write its own tokens (an account a switch
// moved away from) back into auth.json (an in-flight persist_tokens takes no lock). Those tokens may be a rotation of that profile's login, so they
// are first saved into its stored profile when they are newer than what it holds, by the rule the token refresher commits a rotation by
// (usages.CredentialAdvanced); an older or token-only copy leaves the profile as it is. Then the switched-to profile is installed again: on the
// link model the snapshot is written back, on the token-copy model the profile's stored credential is fanned out again (its live copy token-only).
//
// Nothing is done when someone switched again since (profile is no longer active), when the file is gone or names no account (a logout is not
// ours to undo), when it still names the account, or when it names an account no stored profile holds (a login made outside the switcher is not
// ours to undo either). The live file is never read back into the switched-to profile. It reports whether the live login was installed again.
func (m *Manager) ReinstallIfOverwritten(tool, profile, account string, snapshot []byte) (bool, error) {
	if tool != ToolCodex || account == "" || len(snapshot) == 0 {
		return false, nil
	}
	// An unlocked look first, to know whose account lock to take: account locks are taken before the switcher lock, never inside it.
	_, now, err := m.LiveAccount(tool)
	if err != nil || now == "" || now == account {
		return false, nil
	}
	owner := m.storedAccount(tool, profile, now)
	if owner == "" {
		return false, nil
	}
	release, err := usages.HoldMaintenanceProfiles(m.root, tool, []usages.MaintenanceProfile{{Name: owner, CredPath: m.profileCredPath(tool, owner)}}, overwriteAccountWait)
	if err != nil {
		return false, fmt.Errorf("the account lock of %s/%s: %w", tool, owner, err)
	}
	defer release()
	reinstalled := false
	err = m.WithLock(func() error {
		active, err := m.Active(tool)
		if err != nil {
			return err
		}
		if active != profile {
			return nil
		}
		path, err := m.LiveCredentialPath(tool)
		if err != nil {
			return err
		}
		live, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		now := credtarget.CodexAccount(live)
		if now == "" || now == account || m.storedAccount(tool, profile, now) != owner {
			return nil
		}
		if stored, err := m.StoredCredential(tool, owner); err == nil && usages.CredentialAdvanced(tool, stored, live) {
			if err := m.writeUniqueBackup(tool, owner, stored); err != nil {
				return fmt.Errorf("back up %s before saving the tokens written back: %w", owner, err)
			}
			if err := atomicWrite(m.profileCredPath(tool, owner), live, 0o600); err != nil {
				return fmt.Errorf("save the tokens written back into %s: %w", owner, err)
			}
		}
		if m.TokenCopiesFor(tool) {
			full, err := m.StoredCredential(tool, profile)
			if err != nil {
				return fmt.Errorf("read the active profile: %w", err)
			}
			// The live file may hold a full login (a refresh token); it was saved above when it was newer, so it may be replaced.
			replace := m.copyHooks.ReplaceActive
			if replace == nil {
				replace = m.copyHooks.FanOutActive
			}
			if replace == nil {
				return errors.New("install the active profile again: token-copy fan-out is not wired")
			}
			if err := replace(tool, full); err != nil {
				return fmt.Errorf("install the active profile again: %w", err)
			}
		} else if err := atomicWrite(path, snapshot, 0o600); err != nil {
			return fmt.Errorf("install the active profile again: %w", err)
		}
		reinstalled = true
		return nil
	})
	return reinstalled, err
}

// storedAccount names another stored profile of tool than active that holds a login of account ("" when none does). A linked profile (the active
// one's link to the live file) and unreadable ones are skipped.
func (m *Manager) storedAccount(tool, active, account string) string {
	names, err := m.ProfileNames(tool)
	if err != nil {
		return ""
	}
	for _, name := range names {
		if name == active {
			continue
		}
		data, err := m.StoredCredential(tool, name)
		if err != nil {
			continue
		}
		if credtarget.CodexAccount(data) == account {
			return name
		}
	}
	return ""
}
