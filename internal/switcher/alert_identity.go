package switcher

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// EnsureUsageAlertIdentities gives each stored profile an opaque identity that
// moves with its directory on rename and disappears when the profile is removed.
func (m *Manager) EnsureUsageAlertIdentities() error {
	if m.readOnly {
		return errors.New("read-only switcher")
	}
	lock, err := m.acquireLock()
	if err != nil {
		return err
	}
	defer lock.unlock()
	profiles, err := m.List()
	if err != nil {
		return err
	}
	for _, profile := range profiles {
		if m.alertIdentity(profile.Tool, profile.Name) != "" {
			continue
		}
		value := make([]byte, 16)
		if _, err := rand.Read(value); err != nil {
			return err
		}
		if err := atomicWrite(m.alertIdentityPath(profile.Tool, profile.Name), []byte(hex.EncodeToString(value)), 0600); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) alertIdentityPath(tool, name string) string {
	return filepath.Join(m.profileDir(tool, name), "usage-alert-id")
}

func (m *Manager) alertIdentity(tool, name string) string {
	data, err := os.ReadFile(m.alertIdentityPath(tool, name))
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(data))
	if len(value) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}
