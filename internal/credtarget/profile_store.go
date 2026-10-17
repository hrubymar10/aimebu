package credtarget

import (
	"errors"
	"time"
)

// Sentinel errors a ProfileStore returns, so the reconciler can tell them apart
// without importing the switcher (which imports this package).
var (
	// ErrLinkedProfile: a profile's stored credential is still a link to a live
	// file (the pre-migration layout). Its bytes belong to the live file.
	ErrLinkedProfile = errors.New("profile credentials are a link to a live file; run the credential migration first")
	// ErrProfileExists: a new profile's name is taken.
	ErrProfileExists = errors.New("profile already exists")
	// ErrProfileNotFound: a replaced profile has no directory.
	ErrProfileNotFound = errors.New("profile not found")
)

// LoginImport is one full credential that landed in a token-only target (a
// /login), to be stored as a profile's regular credential file.
type LoginImport struct {
	Tool    string
	Profile string
	// Create makes a new profile; false replaces the stored credential of an
	// existing one. Replacing keeps the old bytes as a backup.
	Create bool
	Full   []byte
	// Email is the Claude account identity to record beside the profile (its
	// .claude.json lags credentials, so the profile needs its own copy). Codex
	// carries its email in the id_token and needs none.
	Email string
	// SetActive makes Profile the active profile: the live file now holds its
	// account, and the active pointer is what names that.
	SetActive bool
}

// ProfileStore is what the reconciler needs from the profile store. The
// switcher implements it (switcher.CredStore); tests use a fake.
type ProfileStore interface {
	// Active names the tool's active profile ("" when none).
	Active(tool string) (string, error)
	ProfileNames(tool string) ([]string, error)
	// StoredCredential reads a profile's stored bytes (os.ErrNotExist when
	// empty, ErrLinkedProfile when it is a link to a live file).
	StoredCredential(tool, name string) ([]byte, error)
	// ProfileEmail is the Claude account email recorded beside a profile.
	ProfileEmail(tool, name string) string
	// SaveLoginRescue persists a full credential before it is imported or
	// stripped, so a crash between the steps loses nothing.
	SaveLoginRescue(tool string, data []byte, at time.Time) (string, error)
	// ImportLogin stores the credential as a regular profile file under the
	// switcher lock.
	ImportLogin(in LoginImport) error
	// WithLock runs fn under the switcher lock, the lock a Switch holds while it
	// moves the active pointer and rewrites the live files.
	WithLock(fn func() error) error
	// HoldAccount takes the cross-process account lock of a stored profile and
	// holds it until release is called (usages.ErrMaintenanceBusy after wait).
	HoldAccount(tool, name string, wait time.Duration) (release func(), err error)
}
