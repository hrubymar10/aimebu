package switcher

import (
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// CredStore adapts the profile store to credtarget.ProfileStore, the interface
// the credential reconciler works through. (It lives here because this package
// already imports credtarget; credtarget cannot import the switcher.)
type CredStore struct{ *Manager }

var _ credtarget.ProfileStore = CredStore{}

// NewCredStore wraps a switcher manager.
func NewCredStore(m *Manager) CredStore { return CredStore{Manager: m} }

// ImportLogin implements credtarget.ProfileStore.
func (s CredStore) ImportLogin(in credtarget.LoginImport) error { return s.Manager.ImportLogin(in) }

// HoldAccount holds the maintenance (account) lock of the stored profile, the
// same lock a refresh, a warmup and a switch hold, so a login import never
// replaces a credential a refresh is rotating.
func (s CredStore) HoldAccount(tool, name string, wait time.Duration) (func(), error) {
	return usages.HoldMaintenance(s.Root(), tool, name, s.StoredCredPath(tool, name), wait)
}
