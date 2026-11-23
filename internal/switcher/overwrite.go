package switcher

import (
	"fmt"
	"os"

	"github.com/hrubymar10/aimebu/v2/internal/credtarget"
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

// ReinstallIfOverwritten puts the snapshot of a Codex switch back when the live file names, instead of the account the switch installed, the
// account of another of the tool's stored profiles: a codex process that was running at the switch can write its own tokens (an account a switch
// moved away from) back into auth.json (an in-flight persist_tokens takes no lock). Nothing is done when someone switched again since (profile is
// no longer active), when the file is gone or names no account (a logout is not ours to undo), when it still names the account, or when it names
// an account no stored profile holds (a login made outside the switcher is not ours to undo either). The stored profile copies are never read
// back from the live file here, so the old account's tokens never reach a stored profile through this path. It reports whether the snapshot was
// written.
func (m *Manager) ReinstallIfOverwritten(tool, profile, account string, snapshot []byte) (bool, error) {
	if tool != ToolCodex || account == "" || len(snapshot) == 0 {
		return false, nil
	}
	reinstalled := false
	err := m.WithLock(func() error {
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
		if now == "" || now == account || !m.storedAccount(tool, profile, now) {
			return nil
		}
		if err := atomicWrite(path, snapshot, 0o600); err != nil {
			return fmt.Errorf("install the active profile again: %w", err)
		}
		reinstalled = true
		return nil
	})
	return reinstalled, err
}

// storedAccount says another stored profile of tool than active holds a login of account. A linked profile (the active one's link to the live
// file) and unreadable ones are skipped.
func (m *Manager) storedAccount(tool, active, account string) bool {
	names, err := m.ProfileNames(tool)
	if err != nil {
		return false
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
			return true
		}
	}
	return false
}
