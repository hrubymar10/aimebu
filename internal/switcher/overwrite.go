package switcher

import (
	"fmt"
	"os"

	"github.com/hrubymar10/aimebu/v2/internal/credtarget"
)

// LiveSnapshot is what a switch of tool installed, as the live credential file holds it right after the switch: its bytes and the account they name.
// ReinstallIfOverwritten takes both back. Only Codex has the problem they exist for. An account is "" (and the check is skipped) when the file does
// not name one.
func (m *Manager) LiveSnapshot(tool string) (data []byte, account string, err error) {
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

// ReinstallIfOverwritten puts the snapshot of a Codex switch back when the live file no longer names the account it installed. A codex process
// that was running at the switch can write its own tokens (the old account's) back into auth.json (an in-flight persist_tokens takes no lock). Nothing
// is done when someone switched again since (profile is no longer active), when the file is gone or names no account (a logout is not ours to undo),
// or when it still names the account. The stored profile copies are never read back from the live file here, so the old account's tokens never reach
// a stored profile through this path. It reports whether the snapshot was written.
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
		if now == "" || now == account {
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
