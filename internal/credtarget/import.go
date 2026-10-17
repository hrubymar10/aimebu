package credtarget

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/usages"
)

// Import reasons, in the order ChooseImport tries them.
const (
	ReasonActiveEmpty  = "active-empty"
	ReasonSameAccount  = "same-account"
	ReasonEmailMatch   = "email-match"
	ReasonNewLogin     = "new-login"
	newLoginNamePrefix = "login-"
)

// ImportDecision is where a full credential found in a target goes (C3).
type ImportDecision struct {
	Profile string // profile to import into / replace
	Create  bool   // true when a new `login-<ts>` profile must be created
	Reason  string // "active-empty" | "same-account" | "email-match" | "new-login"
	// Note says why a login became a new profile when a match was expected, e.g.
	// two profiles claimed the same account. It goes into the alert text.
	Note string
}

// ImportEnv gives ChooseImport read access to what it needs, so it is testable
// without a switcher.
type ImportEnv struct {
	Tool          string
	ActiveProfile string // "" when none
	ActiveEmpty   bool   // the active profile has no stored credentials
	// LiveAccount identifies the account of the credential found in the target:
	// claude: email from usages.ClaudeAccountFilePath(home) (~/.claude.json, or
	// $CLAUDE_CONFIG_DIR/.claude.json); codex: id_token email / account_id.
	LiveAccount string
	// ProfileAccount maps profile name -> account identity (same derivation).
	ProfileAccount map[string]string
	// Now names a new login profile. The zero value means time.Now().
	Now time.Time
}

// ChooseImport implements the C3 order: (1) active profile empty -> import
// there; (2) the credential's account matches the active profile -> replace it
// (CAS); (3) email-match any other profile -> replace it and alert; (4) else
// create `login-<ts>` and alert. It never overwrites another account's profile
// without a match.
//
// An unknown account (empty LiveAccount, or a profile whose account could not
// be read) never matches: claiming "same account" without proof would
// overwrite another account's login, so the credential becomes a new profile.
//
// Owner: otto. Tests: T4 matrix (empty active, same account, other known account,
// unknown account, codex variants).
func ChooseImport(env ImportEnv) (ImportDecision, error) {
	if env.Tool != ToolClaude && env.Tool != ToolCodex {
		return ImportDecision{}, fmt.Errorf("credtarget: unknown tool %q", env.Tool)
	}
	if env.ActiveProfile != "" && env.ActiveEmpty {
		return ImportDecision{Profile: env.ActiveProfile, Reason: ReasonActiveEmpty}, nil
	}
	live := AccountKey(env.LiveAccount)
	if live != "" {
		if env.ActiveProfile != "" && AccountKey(env.ProfileAccount[env.ActiveProfile]) == live {
			return ImportDecision{Profile: env.ActiveProfile, Reason: ReasonSameAccount}, nil
		}
		var matches []string
		for name, account := range env.ProfileAccount {
			if name != env.ActiveProfile && AccountKey(account) == live {
				matches = append(matches, name)
			}
		}
		switch len(matches) {
		case 1:
			return ImportDecision{Profile: matches[0], Reason: ReasonEmailMatch}, nil
		case 0:
		default:
			// Two profiles hold the same account: replacing either would
			// silently pick one, so keep the login on its own.
			return newLoginDecision(env, fmt.Sprintf("%d profiles hold this account", len(matches))), nil
		}
	}
	note := ""
	if live == "" {
		note = "the login's account could not be identified"
	}
	return newLoginDecision(env, note), nil
}

func newLoginDecision(env ImportEnv, note string) ImportDecision {
	return ImportDecision{Profile: NewLoginProfileName(env.Now), Create: true, Reason: ReasonNewLogin, Note: note}
}

// NewLoginProfileName is the profile a login of an unknown account lands in.
func NewLoginProfileName(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	return newLoginNamePrefix + now.UTC().Format("20060102T150405")
}

// AccountKey normalises an account identity for comparison.
func AccountKey(account string) string { return strings.ToLower(strings.TrimSpace(account)) }

// ClaudeAccountEmail reads oauthAccount.emailAddress from a claude account-state
// file (usages.ClaudeAccountFilePath). It is best effort: a missing or unreadable
// file means "unknown", never an error. The file is a large junk drawer that is
// only ever read here.
func ClaudeAccountEmail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var file struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &file) != nil {
		return ""
	}
	return file.OAuthAccount.EmailAddress
}

// ClaudeAccountSource is one claude account-state file that names an account.
type ClaudeAccountSource struct {
	Path    string
	Email   string
	ModTime time.Time
}

// ClaudeAccountSources lists the account-state files that exist and name an
// account for a Claude credentials file: the one beside the credentials
// (`<dir>/.claude.json`, which sessions started with CLAUDE_CONFIG_DIR use) and
// claude-code's own default for this environment (usages.ClaudeAccountFilePath:
// the home root unless CLAUDE_CONFIG_DIR is set in THIS process). The two can
// differ, and which one is real depends on how the sessions were started, not on
// how the server was: on a host whose sessions run with CLAUDE_CONFIG_DIR set,
// the home-root file is a stub while the real one sits in the config directory.
func ClaudeAccountSources(credPath, home string) []ClaudeAccountSource {
	candidates := []string{filepath.Join(filepath.Dir(credPath), ".claude.json")}
	if def := usages.ClaudeAccountFilePath(home); def != candidates[0] {
		candidates = append(candidates, def)
	}
	var sources []ClaudeAccountSource
	for _, path := range candidates {
		email := ClaudeAccountEmail(path)
		if email == "" {
			continue
		}
		var mod time.Time
		if info, err := os.Stat(path); err == nil {
			mod = info.ModTime()
		}
		sources = append(sources, ClaudeAccountSource{Path: path, Email: email, ModTime: mod})
	}
	return sources
}

// ClaudeAccountFileFor picks the account-state file to read for a credentials
// file: the most recently written of the candidates that name an account (a
// /login rewrites the one its process uses, so the newest is the login's), else
// the default location. Callers that identify a Claude account (the reconciler,
// the switcher's profile capture) share it, so they never disagree.
func ClaudeAccountFileFor(credPath, home string) string {
	best := ClaudeAccountSource{}
	for _, source := range ClaudeAccountSources(credPath, home) {
		if best.Path == "" || source.ModTime.After(best.ModTime) {
			best = source
		}
	}
	if best.Path != "" {
		return best.Path
	}
	return usages.ClaudeAccountFilePath(home)
}

// ClaudeAccountFilesDisagree reports the candidate account files when they name
// different accounts, which is what a doctor check should flag: the identity of
// every login depends on which one is read.
func ClaudeAccountFilesDisagree(credPath, home string) ([]ClaudeAccountSource, bool) {
	sources := ClaudeAccountSources(credPath, home)
	for i := 1; i < len(sources); i++ {
		if AccountKey(sources[i].Email) != AccountKey(sources[0].Email) {
			return sources, true
		}
	}
	return sources, false
}

// CodexAccount identifies the account behind a codex auth.json: the id_token
// email when present, otherwise the account id ("acct:<id>"). Empty means unknown.
func CodexAccount(data []byte) string {
	var file struct {
		Tokens struct {
			IDToken   string `json:"id_token"`
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &file) != nil {
		return ""
	}
	if email := usages.CodexIDTokenEmail(file.Tokens.IDToken); email != "" {
		return email
	}
	if file.Tokens.AccountID != "" {
		return "acct:" + file.Tokens.AccountID
	}
	return ""
}

var errNoProfileName = errors.New("credtarget: import decision names no profile")
