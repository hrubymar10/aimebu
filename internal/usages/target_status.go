package usages

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goccy/go-json"
)

// Credential levels of a TargetStatus row.
const (
	TargetLevelOK       = "ok"
	TargetLevelExpiring = "expiring" // less than TargetAlertWindow left
	TargetLevelExpired  = "expired"
	TargetLevelUnknown  = "unknown"
)

// TargetStatus is the state of one credential the token-only model keeps alive:
// a profile in the store (the only full credential) or a live file the CLIs run
// on (a token-only target). It carries no token, ever.
type TargetStatus struct {
	Tool        string     `json:"tool"`
	Profile     string     `json:"profile,omitempty"` // store rows only
	Role        string     `json:"role"`              // "store" | "live"
	Active      bool       `json:"active,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	SecondsLeft *int64     `json:"seconds_left,omitempty"` // negative once expired
	Level       string     `json:"level"`
	// State is the refresher's verdict when the account needs attention:
	// reauth_required, needs_relogin_soon or refresh_failed.
	State       string     `json:"state,omitempty"`
	NextAttempt *time.Time `json:"next_attempt,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}

// CredentialAlert is what the Usages panel and the doctor show a human: one
// short sentence per credential that needs attention.
type CredentialAlert struct {
	Kind    string `json:"kind"` // reauth_required | needs_relogin_soon | refresh_failed | expiring | expired
	Tool    string `json:"tool"`
	Profile string `json:"profile,omitempty"`
	Role    string `json:"role"`
	Message string `json:"message"`
}

// credentialLevel classifies the time left.
func credentialLevel(expiresAt *time.Time, now time.Time) (level string, seconds *int64) {
	if expiresAt == nil {
		return TargetLevelUnknown, nil
	}
	left := int64(expiresAt.Sub(now) / time.Second)
	switch {
	case !now.Before(*expiresAt):
		return TargetLevelExpired, &left
	case expiresAt.Sub(now) < TargetAlertWindow:
		return TargetLevelExpiring, &left
	}
	return TargetLevelOK, &left
}

// tolerantExpiry reads a credential's expiry without demanding a refresh token,
// so a token-only live file (which has none) is judged by its access token.
func tolerantExpiry(tool string, data []byte) *time.Time {
	if tool == "codex" {
		creds, _, err := parseCodexAuthData(data)
		if err == nil {
			return CodexTokenExpiry(creds.AccessToken)
		}
		var raw codexAuthFile
		if json.Unmarshal(data, &raw) != nil {
			return nil
		}
		return CodexTokenExpiry(firstNonEmpty(raw.Tokens.AccessToken, raw.Tokens.AccessTokenC))
	}
	var raw claudeAuthFile
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	tokens := raw.ClaudeAiOAuth
	if firstNonEmpty(tokens.AccessToken, tokens.AccessTokenS) == "" {
		tokens = raw.ClaudeAiOAuthS
	}
	ms := tokens.ExpiresAt
	if ms == 0 {
		ms = tokens.ExpiresAtS
	}
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(int64(ms)).UTC()
	return &t
}

// Statuses lists every profile of the store plus the two live files, each with
// its time left and, for a profile, the refresher's state. It reads files only.
func (r *TargetRefresher) Statuses() []TargetStatus {
	now := r.now()
	var out []TargetStatus
	states := r.AccountStates()
	for _, tool := range []string{"claude", "codex"} {
		if r.toolOn != nil && !r.toolOn(tool) {
			continue // still on the link model: its live file is a full credential the CLI renews itself
		}
		profiles, err := r.rawProfiles(tool)
		if err != nil {
			continue
		}
		for _, p := range profiles {
			if p.CredPath == "" {
				continue
			}
			data, err := os.ReadFile(p.CredPath)
			if err != nil {
				continue
			}
			row := TargetStatus{Tool: tool, Profile: p.Name, Role: "store", Active: p.Active}
			row.ExpiresAt = tolerantExpiry(tool, data)
			row.Level, row.SecondsLeft = credentialLevel(row.ExpiresAt, now)
			row.State = states[tool+"/"+p.Name]
			if token, err := refreshToken(data, tool); err == nil {
				r.mu.Lock()
				if a := r.accounts[tool+"/"+tokenHash(tool, token)]; a != nil {
					if !a.nextTry.IsZero() && a.nextTry.After(now) {
						t := a.nextTry.UTC()
						row.NextAttempt = &t
					}
					row.Detail = a.detail
				}
				r.mu.Unlock()
				if entry, ok := r.stateStore().account(tool, tokenHash(tool, token)); ok {
					row.Detail = entry.Detail
				}
			}
			out = append(out, row)
		}
		livePath := ""
		if tool == "codex" {
			livePath, _ = codexAuthPath()
		} else {
			livePath, _ = claudeAuthPath()
		}
		if data, err := os.ReadFile(livePath); err == nil {
			row := TargetStatus{Tool: tool, Role: "live", Active: true}
			row.ExpiresAt = tolerantExpiry(tool, data)
			row.Level, row.SecondsLeft = credentialLevel(row.ExpiresAt, now)
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tool != out[j].Tool {
			return out[i].Tool < out[j].Tool
		}
		if out[i].Role != out[j].Role {
			return out[i].Role > out[j].Role // live first
		}
		return out[i].Profile < out[j].Profile
	})
	return out
}

// Alerts turns the statuses into the sentences a human has to act on. Only the
// credentials that matter right now raise one: anything with a refresher state,
// and any live file or active profile that is expiring or expired. An idle
// profile inside its refresh lead is normal and stays quiet until it is within
// the alert window.
func (r *TargetRefresher) Alerts() []CredentialAlert {
	var out []CredentialAlert
	for _, s := range r.Statuses() {
		who := s.Tool + " " + s.Role
		if s.Profile != "" {
			who = s.Tool + " profile " + s.Profile
		}
		switch {
		case s.State == AccountReauthRequired:
			out = append(out, CredentialAlert{Kind: s.State, Tool: s.Tool, Profile: s.Profile, Role: s.Role, Message: who + ": the provider rejected the refresh token. Log in again."})
		case s.State == AccountNeedsReloginSoon:
			out = append(out, CredentialAlert{Kind: s.State, Tool: s.Tool, Profile: s.Profile, Role: s.Role, Message: who + ": a refresh may have been spent without a result, so it will not be retried. Log in again before it expires."})
		case s.State == AccountRefreshFailed:
			out = append(out, CredentialAlert{Kind: s.State, Tool: s.Tool, Profile: s.Profile, Role: s.Role, Message: who + ": refreshing keeps failing; the server log has the detail."})
		}
		switch s.Level {
		case TargetLevelExpired:
			out = append(out, CredentialAlert{Kind: "expired", Tool: s.Tool, Profile: s.Profile, Role: s.Role, Message: who + ": the access token has expired."})
		case TargetLevelExpiring:
			out = append(out, CredentialAlert{Kind: "expiring", Tool: s.Tool, Profile: s.Profile, Role: s.Role, Message: fmt.Sprintf("%s: the access token expires in %d min.", who, max(int(*s.SecondsLeft/60), 0))})
		}
	}
	return out
}

// emitExpiryAlerts raises the edge alerts for credentials close to expiry, once
// per credential and expiry time, so a human hears about them while the server
// is still able to say so.
func (r *TargetRefresher) emitExpiryAlerts() {
	for _, s := range r.Statuses() {
		if s.Level != TargetLevelExpiring && s.Level != TargetLevelExpired || s.ExpiresAt == nil {
			continue
		}
		key := fmt.Sprintf("%s/%s/%s@%d", s.Tool, s.Role, s.Profile, s.ExpiresAt.Unix())
		who := s.Tool + " " + s.Role
		if s.Profile != "" {
			who = s.Tool + " profile " + s.Profile
		}
		detail := "the access token expires in under 15 minutes"
		if s.Level == TargetLevelExpired {
			detail = "the access token has expired"
		}
		r.emit(s.Level, key, who+": "+detail)
	}
}

// credentialAlerts is the Manager-side accessor: nil unless the refresher is on.
func (m *Manager) credentialAlerts() []CredentialAlert {
	if !m.targetRefresherOwnsRefresh() {
		return nil
	}
	return m.targetRefresher.Alerts()
}

func (r Routes) handleCredentialTargets(w http.ResponseWriter, _ *http.Request) {
	body := map[string]any{"enabled": false, "targets": []TargetStatus{}, "alerts": []CredentialAlert{}}
	if r.Manager != nil && r.Manager.targetRefresherOwnsRefresh() {
		body["enabled"] = true
		// Detail is CLI output and error text: it stays in the state file, the
		// log and the owner's doctor run, never in an API any client can read.
		targets := r.Manager.targetRefresher.Statuses()
		for i := range targets {
			targets[i].Detail = ""
		}
		body["targets"] = targets
		body["alerts"] = r.Manager.targetRefresher.Alerts()
	}
	writeUsageJSON(w, body, nil)
}

// CredentialExpiry reads the expiry of a credential document of tool without
// requiring a refresh token (so token-only live files work). Nil when unknown.
func CredentialExpiry(tool string, data []byte) *time.Time { return tolerantExpiry(tool, data) }

// LiveCredentialPath is the live credential file of tool ("claude" or "codex"),
// honouring CLAUDE_CONFIG_DIR / CODEX_HOME.
func LiveCredentialPath(tool string) (string, error) {
	if tool == "codex" {
		return codexAuthPath()
	}
	return claudeAuthPath()
}

// CredentialReport judges every credential of the token-only model from files
// alone, so `aimebu doctor` can run it with the server stopped (the moment it
// matters most): each profile of the store under switcherRoot, and the two live
// files. State comes from refresh-state.json, matched to the refresh token a
// profile holds now.
func CredentialReport(switcherRoot string, now time.Time) []TargetStatus {
	var out []TargetStatus
	active := map[string]string{}
	if raw, err := os.ReadFile(filepath.Join(switcherRoot, "state.json")); err == nil {
		var st struct {
			Active map[string]string `json:"active"`
		}
		if json.Unmarshal(raw, &st) == nil && st.Active != nil {
			active = st.Active
		}
	}
	store := newRefreshStateStore(switcherRoot)
	for _, tool := range []string{"claude", "codex"} {
		file := ".credentials.json"
		if tool == "codex" {
			file = "auth.json"
		}
		entries, _ := os.ReadDir(filepath.Join(switcherRoot, "profiles", tool))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(switcherRoot, "profiles", tool, e.Name(), file))
			if err != nil {
				continue
			}
			row := TargetStatus{Tool: tool, Profile: e.Name(), Role: "store", Active: active[tool] == e.Name()}
			row.ExpiresAt = tolerantExpiry(tool, data)
			row.Level, row.SecondsLeft = credentialLevel(row.ExpiresAt, now)
			if token, err := refreshToken(data, tool); err == nil {
				if entry, ok := store.account(tool, tokenHash(tool, token)); ok {
					row.State, row.Detail = entry.State, entry.Detail
				}
			}
			out = append(out, row)
		}
		if path, err := LiveCredentialPath(tool); err == nil {
			if data, err := os.ReadFile(path); err == nil {
				row := TargetStatus{Tool: tool, Role: "live", Active: true}
				row.ExpiresAt = tolerantExpiry(tool, data)
				row.Level, row.SecondsLeft = credentialLevel(row.ExpiresAt, now)
				out = append(out, row)
			}
		}
	}
	return out
}
