package switcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages"
)

const (
	ToolClaude = "claude"
	ToolCodex  = "codex"
)

// Tools returns the set of tools the switcher supports.
func Tools() []string { return []string{ToolClaude, ToolCodex} }

// Sentinel errors — callers branch on behaviour with errors.Is, never string matching.
var (
	ErrDisabled           = errors.New("switcher is disabled")
	ErrNotEligible        = errors.New("tool is not eligible for switching")
	ErrNoActiveProfile    = errors.New("no active profile to capture into")
	ErrProfileNotFound    = errors.New("profile not found")
	ErrProfileExists      = errors.New("profile already exists")
	ErrInvalidCredentials = errors.New("credentials failed validation")
	ErrActiveProfile      = errors.New("profile is active; pass force to remove")
	ErrInvalidName        = errors.New("invalid profile name")
)

// NotEligibleError is the concrete type returned when a tool's live credentials
// are not usable for switching. Reason is safe to display verbatim — it never
// contains credential material, only state descriptions ("not logged in", etc.).
// errors.Is(err, ErrNotEligible) works via the Is method; callers that need the
// human-readable text use errors.As to pull the Reason field directly.
type NotEligibleError struct {
	Tool   string
	Reason string
}

func (e *NotEligibleError) Error() string        { return e.Reason }
func (e *NotEligibleError) Is(target error) bool { return target == ErrNotEligible }

// Manager is the switcher core library. Zero server dependency — no SQLite,
// no HTTP, no daemon. The HTTP handler (S3) and the CLI (S2) are thin wrappers.
type Manager struct {
	root    string // <config>/switcher
	homeDir string // overrides os.UserHomeDir() in tests; empty in production
}

// DefaultRoot resolves the switcher root from AIMEBU_CONFIG_DIR (or the default
// home-based path) and returns <config>/switcher.
func DefaultRoot() (string, error) {
	dir := os.Getenv("AIMEBU_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".aimebu")
	}
	return filepath.Join(dir, "switcher"), nil
}

// New creates a Manager rooted at root (typically from DefaultRoot).
// Directories are created lazily; New itself does no I/O.
func New(root string) (*Manager, error) {
	return &Manager{root: root}, nil
}

// Enabled reads the enabled flag from switcher/state.json. Works with the server
// stopped — no daemon, no SQLite, no usages dependency.
func (m *Manager) Enabled() (bool, error) {
	state, err := m.loadState()
	if err != nil {
		return false, err
	}
	return state.Enabled, nil
}

// SetHomeDir overrides the home directory used for live credential paths.
// Production code resolves via os.UserHomeDir; tests inject a temp directory
// so the real ~/.claude and ~/.codex are never touched.
func (m *Manager) SetHomeDir(homeDir string) {
	m.homeDir = homeDir
}

// WithLock acquires the switcher flock, calls fn, and releases the lock.
// External callers (e.g. the usages poller) use this to hold switcher/.lock
// around writes to the live credentials path so a concurrent switch cannot
// silently overwrite the newly-active profile's tokens (§14.2).
func (m *Manager) WithLock(fn func() error) error {
	lock, err := m.acquireLock()
	if err != nil {
		return fmt.Errorf("acquire switcher lock: %w", err)
	}
	defer func() { _ = lock.unlock() }()
	return fn()
}

// SetEnabled writes the enabled flag into switcher/state.json under switcher/.lock.
// The flag lives in the same file as the active-profile pointers so Enabled() and
// Switch() both read a single consistent snapshot with one lock.
func (m *Manager) SetEnabled(v bool) error {
	lock, lockErr := m.acquireLock()
	if lockErr != nil {
		return fmt.Errorf("acquire switcher lock: %w", lockErr)
	}
	defer lock.unlock()

	state, err := m.loadState()
	if err != nil {
		return err
	}
	state.Enabled = v
	return m.saveState(state)
}

// ResolveHome returns the effective home directory: homeDir override (tests)
// or os.UserHomeDir() (production).
func (m *Manager) ResolveHome() (string, error) {
	if m.homeDir != "" {
		return m.homeDir, nil
	}
	return os.UserHomeDir()
}

// liveCredPath returns the live credentials file path for tool.
func (m *Manager) liveCredPath(tool string) (string, error) {
	home, err := m.ResolveHome()
	if err != nil {
		return "", err
	}
	switch tool {
	case ToolClaude:
		return usages.ClaudeLiveCredPath(home), nil
	case ToolCodex:
		return filepath.Join(home, ".codex", "auth.json"), nil
	default:
		return "", fmt.Errorf("unknown tool %q", tool)
	}
}

// liveCredDir returns the directory containing the live credentials file.
func (m *Manager) liveCredDir(tool string) (string, error) {
	p, err := m.liveCredPath(tool)
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// profileCredFilename returns the credential filename stored inside a profile directory.
func profileCredFilename(tool string) string {
	switch tool {
	case ToolClaude:
		return ".credentials.json"
	case ToolCodex:
		return "auth.json"
	default:
		return ""
	}
}

// Root returns the switcher root directory (e.g. ~/.aimebu/switcher).
func (m *Manager) Root() string { return m.root }

// StoredCredPath returns the path to the credential file for a stored profile.
// Callers that need to read a non-active profile's credentials use this path.
func (m *Manager) StoredCredPath(tool, name string) string {
	return m.profileCredPath(tool, name)
}

// profilesRoot returns <root>/profiles.
func (m *Manager) profilesRoot() string { return filepath.Join(m.root, "profiles") }

// profileDir returns the directory for a specific (tool, name) profile.
func (m *Manager) profileDir(tool, name string) string {
	return filepath.Join(m.profilesRoot(), tool, name)
}

// profileCredPath returns the stored credential file path for a profile.
func (m *Manager) profileCredPath(tool, name string) string {
	return filepath.Join(m.profileDir(tool, name), profileCredFilename(tool))
}

// backupsRoot returns <root>/backups.
func (m *Manager) backupsRoot() string { return filepath.Join(m.root, "backups") }

// backupPath returns the path for a pre-switch backup. unixSec is embedded in
// the filename so consecutive backups of the same profile never collide.
func (m *Manager) backupPath(tool, name string, unixSec int64) string {
	return filepath.Join(m.backupsRoot(), tool, fmt.Sprintf("%s-%d.json", name, unixSec))
}

// lockPath returns the flock file for the switcher.
func (m *Manager) lockPath() string { return filepath.Join(m.root, ".lock") }

// validateProfileName rejects anything that is not a single plain path element,
// so a config-sourced name cannot escape the switcher root via ".." or a separator.
func validateProfileName(name string) error {
	switch name {
	case "", ".", "..":
		return fmt.Errorf("%w: %q is not a plain path element", ErrInvalidName, name)
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidName, name)
	}
	if strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("%w: %q contains a null byte", ErrInvalidName, name)
	}
	if filepath.Clean(name) != name {
		return fmt.Errorf("%w: %q is not a clean path element", ErrInvalidName, name)
	}
	return nil
}

// profileMetaPath is the per-profile sidecar holding account identity that the
// credential file itself does not carry.
func (m *Manager) profileMetaPath(tool, name string) string {
	return filepath.Join(m.profileDir(tool, name), "profile.json")
}

// profileMeta is account identity captured alongside a profile's credentials.
// Claude's email lives in .claude.json, a single global file that does NOT move
// with credentials — so without this sidecar every claude profile would report
// whichever account is currently live. Codex needs no sidecar: its email is in
// the profile's own id_token.
type profileMeta struct {
	Email string `json:"email,omitempty"`
	// StaleEmail is the address .claude.json held at the moment this profile
	// became active — i.e. the PREVIOUS account's, because that file lags the
	// credential swap until the harness re-authenticates. A live value equal to
	// this one proves nothing has authenticated yet, so it must never be adopted
	// as this profile's identity.
	StaleEmail string `json:"stale_email,omitempty"`
}

func (m *Manager) readProfileMeta(tool, name string) profileMeta {
	var meta profileMeta
	data, err := os.ReadFile(m.profileMetaPath(tool, name))
	if err != nil {
		return meta
	}
	_ = json.Unmarshal(data, &meta)
	return meta
}

// writeProfileMeta records the live account identity for a profile. Best effort:
// a failure here must never fail a switch or an import, since the credentials
// are the thing that matters and the email is only a label.
func (m *Manager) writeProfileMeta(tool, name string, meta profileMeta) {
	if meta.Email == "" && meta.StaleEmail == "" {
		_ = os.Remove(m.profileMetaPath(tool, name))
		return
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return
	}
	_ = atomicWrite(m.profileMetaPath(tool, name), data, 0o600)
}

// claudeEmailFromDotJSON reads oauthAccount.emailAddress from claude-code's
// account-state file, whose location follows CLAUDE_CONFIG_DIR (see
// usages.ClaudeAccountFilePath). Returns "" on any error — best effort.
// Never writes the file: it is a ~70-key junk drawer; reading one key is safe.
func (m *Manager) claudeEmailFromDotJSON() string {
	home, err := m.ResolveHome()
	if err != nil {
		return ""
	}
	data, readErr := os.ReadFile(usages.ClaudeAccountFilePath(home))
	if readErr != nil {
		return ""
	}
	var f struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	return f.OAuthAccount.EmailAddress
}
