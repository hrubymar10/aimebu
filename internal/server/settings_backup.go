package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/quota"
	"github.com/hrubymar10/aimebu/internal/store"
)

// A settings backup is the owner's instance-level choices as one JSON file: what they would want back after a
// reinstall or on a second host. It is built field by field from the store, never from a table dump, so a new column
// can never leak into it; credentials, tokens, provider secrets, machines, users and sessions are not in it.
const (
	settingsBackupSchema  = "aimebu-settings-backup"
	settingsBackupVersion = 1
	settingsBackupLimit   = 4 << 20
)

type backupRole struct {
	Key         string `json:"role_key"`
	Label       string `json:"label"`
	Emoji       string `json:"emoji"`
	Body        string `json:"body"`
	Cardinality string `json:"cardinality"`
	Lead        bool   `json:"cap_is_lead"`
	CanSpawn    bool   `json:"cap_can_spawn"`
	Writer      bool   `json:"cap_singleton_writer"`
}

type backupRetention struct {
	MessagesConsideredExpiredAfterSeconds int `json:"messages_considered_expired_after_seconds"`
}

type settingsBackup struct {
	Schema        string                       `json:"schema"`
	Version       int                          `json:"version"`
	ExportedAt    string                       `json:"exported_at,omitempty"`
	Macros        map[string]string            `json:"macros,omitempty"`
	Roles         []backupRole                 `json:"roles,omitempty"`
	Retention     *backupRetention             `json:"retention,omitempty"`
	QuotaSwitcher map[string]quota.Config      `json:"quota_switcher,omitempty"`
	LimitGuard    map[string]quota.GuardConfig `json:"limit_guard,omitempty"`
	// UIPrefs are browser preferences the web UI adds on export and applies itself on import; the server only
	// checks their shape and counts them.
	UIPrefs map[string]string `json:"ui_prefs,omitempty"`
}

// backupSections is the order sections are listed and applied in: teams after the roles they name.
var backupSections = []string{"macros", "retention", "quota_switcher", "limit_guard", "roles", "ui_prefs"}

var backupUIPrefNames = map[string]bool{"theme": true, "inbox_muted": true, "inbox_sound": true, "inbox_volume": true,
	"rooms_collapsed": true, "usages_group": true, "left_collapsed": true, "right_collapsed": true}

type backupIssue struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

type backupSectionPreview struct {
	Section     string        `json:"section"`
	New         int           `json:"new"`
	Overwrite   int           `json:"overwrite"`
	Unchanged   int           `json:"unchanged"`
	Invalid     []backupIssue `json:"invalid,omitempty"`
	Unavailable string        `json:"unavailable,omitempty"`
}

type backupPreview struct {
	Sections []backupSectionPreview `json:"sections"`
	Ignored  []string               `json:"ignored,omitempty"`
	Digest   string                 `json:"digest"`
}

type backupFailure struct {
	Section string `json:"section"`
	Key     string `json:"key"`
	Error   string `json:"error"`
}

type backupSectionResult struct {
	Section string `json:"section"`
	Applied int    `json:"applied"`
}

type backupResult struct {
	Sections []backupSectionResult `json:"sections"`
	Failed   []backupFailure       `json:"failed,omitempty"`
	Complete bool                  `json:"complete"`
}

// backupPlan is a preview together with what applying it would do.
type backupPlan struct {
	preview backupPreview
	run     map[string]func(context.Context) (int, []backupFailure)
	keys    map[string][]string
	lines   []string
}

func (s *Server) exportSettingsBackup(ctx context.Context, user string) (settingsBackup, error) {
	b := settingsBackup{Schema: settingsBackupSchema, Version: settingsBackupVersion, ExportedAt: time.Now().UTC().Format(time.RFC3339)}
	macros, err := s.Store.ListMacros(ctx)
	if err != nil {
		return b, err
	}
	if len(macros) > 0 {
		b.Macros = macros
	}
	roles, err := s.Store.ListRoles(ctx)
	if err != nil {
		return b, err
	}
	for _, role := range roles {
		b.Roles = append(b.Roles, backupRole{Key: role.Key, Label: role.Label, Emoji: role.Emoji, Body: role.Body, Cardinality: role.Cardinality,
			Lead: role.Lead, CanSpawn: role.CanSpawn, Writer: role.Writer})
	}
	seconds, err := s.Store.MessageExpirySeconds(ctx)
	if err != nil {
		return b, err
	}
	b.Retention = &backupRetention{MessagesConsideredExpiredAfterSeconds: seconds}
	// The quota switcher and the limit guard decide over the owner's logins: an admin's export leaves them out (an admin cannot apply them).
	ownerExport, err := s.Store.IsGlobalOwner(ctx, user)
	if err != nil {
		return b, err
	}
	if runner := s.quotaRunner(); runner != nil && ownerExport {
		overview, err := runner.Overview(ctx)
		if err != nil {
			return b, err
		}
		for tool, view := range overview.Tools {
			if view.Saved {
				if b.QuotaSwitcher == nil {
					b.QuotaSwitcher = map[string]quota.Config{}
				}
				b.QuotaSwitcher[tool] = view.Config
			}
		}
	}
	if guard := s.limitGuard(); guard != nil && ownerExport {
		views, err := guard.Overview(ctx)
		if err != nil {
			return b, err
		}
		for tool, view := range views {
			if view.Saved {
				if b.LimitGuard == nil {
					b.LimitGuard = map[string]quota.GuardConfig{}
				}
				b.LimitGuard[tool] = view.Config
			}
		}
	}
	return b, nil
}

func lineOf(parts ...string) string { return strings.Join(parts, "\x00") }

func hashOf(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// planSettingsBackup compares b with the current state and builds both the preview and the steps that apply it.
func (s *Server) planSettingsBackup(ctx context.Context, user string, b settingsBackup) (*backupPlan, error) {
	p := &backupPlan{run: map[string]func(context.Context) (int, []backupFailure){}, keys: map[string][]string{}}
	sections := map[string]*backupSectionPreview{}
	for _, name := range backupSections {
		sections[name] = &backupSectionPreview{Section: name}
	}
	// Each step is recorded with a hash of the entry it would replace (or "absent"), so a change to that entry after the preview, as well
	// as a different file, changes the digest and the apply is refused.
	note := func(section, kind, key, payload, current string) {
		p.lines = append(p.lines, lineOf(section, kind, key, payload, current))
		p.keys[section] = append(p.keys[section], key)
	}

	// Macros: merged into the current map in one replace.
	macros, err := s.Store.ListMacros(ctx)
	if err != nil {
		return nil, err
	}
	incomingMacros := map[string]string{}
	for _, key := range sortedKeys(b.Macros) {
		body := b.Macros[key]
		if err := store.ValidateMacros(map[string]string{key: body}); err != nil {
			sections["macros"].Invalid = append(sections["macros"].Invalid, backupIssue{Key: key, Reason: err.Error()})
			continue
		}
		switch current, ok := macros[key]; {
		case !ok:
			sections["macros"].New++
			note("macros", "new", key, hashOf(body), "absent")
		case current != body:
			sections["macros"].Overwrite++
			note("macros", "overwrite", key, hashOf(body), hashOf(macros[key]))
		default:
			sections["macros"].Unchanged++
			continue
		}
		incomingMacros[key] = body
	}
	if len(incomingMacros) > 0 {
		merged := map[string]string{}
		for key, body := range macros {
			merged[key] = body
		}
		for key, body := range incomingMacros {
			merged[key] = body
		}
		if err := store.ValidateMacros(merged); err != nil {
			sections["macros"].Invalid = append(sections["macros"].Invalid, backupIssue{Key: "(all)", Reason: err.Error()})
			sections["macros"].New, sections["macros"].Overwrite = 0, 0
			p.keys["macros"], p.lines = nil, dropSection(p.lines, "macros")
		} else {
			p.run["macros"] = func(ctx context.Context) (int, []backupFailure) {
				current, err := s.Store.ListMacros(ctx)
				if err != nil {
					return 0, []backupFailure{{Section: "macros", Key: "(all)", Error: err.Error()}}
				}
				for key, body := range incomingMacros {
					current[key] = body
				}
				if err := s.Store.ReplaceMacros(ctx, current); err != nil {
					return 0, []backupFailure{{Section: "macros", Key: "(all)", Error: err.Error()}}
				}
				return len(incomingMacros), nil
			}
		}
	}

	// Retention.
	if b.Retention != nil {
		seconds := b.Retention.MessagesConsideredExpiredAfterSeconds
		current, err := s.Store.MessageExpirySeconds(ctx)
		if err != nil {
			return nil, err
		}
		switch {
		case seconds != 0 && (seconds < store.MinMessageExpirySeconds || seconds > store.MaxMessageExpirySeconds):
			sections["retention"].Invalid = append(sections["retention"].Invalid, backupIssue{Key: "messages_considered_expired_after_seconds", Reason: "outside the allowed window"})
		case seconds == current:
			sections["retention"].Unchanged++
		default:
			sections["retention"].Overwrite++
			note("retention", "overwrite", "messages", fmt.Sprint(seconds), fmt.Sprint(current))
			p.run["retention"] = func(ctx context.Context) (int, []backupFailure) {
				if err := s.Store.SetMessageExpirySeconds(ctx, seconds); err != nil {
					return 0, []backupFailure{{Section: "retention", Key: "messages", Error: err.Error()}}
				}
				return 1, nil
			}
		}
	}

	// The quota switcher's per-tool configuration, when the switcher is running.
	if len(b.QuotaSwitcher) > 0 {
		runner := s.quotaRunner()
		if runner == nil {
			sections["quota_switcher"].Unavailable = "the quota switcher is not running on this instance"
		} else {
			overview, err := runner.Overview(ctx)
			if err != nil {
				return nil, err
			}
			changed := map[string]quota.Config{}
			for _, tool := range sortedKeys(b.QuotaSwitcher) {
				view, known := overview.Tools[tool]
				switch {
				case !known:
					sections["quota_switcher"].Invalid = append(sections["quota_switcher"].Invalid, backupIssue{Key: tool, Reason: "unknown tool"})
				case view.Saved && hashOf(view.Config) == hashOf(b.QuotaSwitcher[tool]):
					sections["quota_switcher"].Unchanged++
				default:
					if view.Saved {
						sections["quota_switcher"].Overwrite++
					} else {
						sections["quota_switcher"].New++
					}
					changed[tool] = b.QuotaSwitcher[tool]
					note("quota_switcher", "set", tool, hashOf(b.QuotaSwitcher[tool]), func() string {
						if view.Saved {
							return hashOf(view.Config)
						}
						return "absent"
					}())
				}
			}
			if len(changed) > 0 {
				p.run["quota_switcher"] = func(ctx context.Context) (int, []backupFailure) {
					var failed []backupFailure
					applied := 0
					for _, tool := range sortedKeys(changed) {
						if _, err := runner.SaveConfig(ctx, tool, changed[tool]); err != nil {
							failed = append(failed, backupFailure{Section: "quota_switcher", Key: tool, Error: err.Error()})
						} else {
							applied++
						}
					}
					return applied, failed
				}
			}
		}
	}

	// The limit guard's per-tool thresholds, when it is running (it starts with the quota switcher).
	if len(b.LimitGuard) > 0 {
		guard := s.limitGuard()
		if guard == nil {
			sections["limit_guard"].Unavailable = "the limit guard is not running on this instance"
		} else {
			views, err := guard.Overview(ctx)
			if err != nil {
				return nil, err
			}
			changed := map[string]quota.GuardConfig{}
			for _, tool := range sortedKeys(b.LimitGuard) {
				view, known := views[tool]
				switch {
				case !known:
					sections["limit_guard"].Invalid = append(sections["limit_guard"].Invalid, backupIssue{Key: tool, Reason: "unknown tool"})
				case view.Saved && hashOf(view.Config) == hashOf(b.LimitGuard[tool]):
					sections["limit_guard"].Unchanged++
				default:
					if view.Saved {
						sections["limit_guard"].Overwrite++
					} else {
						sections["limit_guard"].New++
					}
					changed[tool] = b.LimitGuard[tool]
					note("limit_guard", "set", tool, hashOf(b.LimitGuard[tool]), func() string {
						if view.Saved {
							return hashOf(view.Config)
						}
						return "absent"
					}())
				}
			}
			if len(changed) > 0 {
				p.run["limit_guard"] = func(ctx context.Context) (int, []backupFailure) {
					var failed []backupFailure
					applied := 0
					for _, tool := range sortedKeys(changed) {
						if _, err := guard.Save(ctx, tool, changed[tool]); err != nil {
							failed = append(failed, backupFailure{Section: "limit_guard", Key: tool, Error: err.Error()})
						} else {
							applied++
						}
					}
					return applied, failed
				}
			}
		}
	}

	// Roles: matched by key.
	if len(b.Roles) > 0 {
		current, err := s.Store.ListRoles(ctx)
		if err != nil {
			return nil, err
		}
		byKey := map[string]store.RoleDefinition{}
		for _, role := range current {
			byKey[role.Key] = role
		}
		var steps []backupRole
		seen := map[string]bool{}
		for _, role := range b.Roles {
			switch {
			case !store.ValidRoleKey(role.Key):
				sections["roles"].Invalid = append(sections["roles"].Invalid, backupIssue{Key: role.Key, Reason: "the role key is not valid"})
				continue
			case seen[role.Key]:
				sections["roles"].Invalid = append(sections["roles"].Invalid, backupIssue{Key: role.Key, Reason: "listed more than once"})
				continue
			case !store.ValidRoleFields(role.Label, role.Emoji, role.Body, role.Cardinality):
				sections["roles"].Invalid = append(sections["roles"].Invalid, backupIssue{Key: role.Key, Reason: "label, emoji, body or cardinality is out of range"})
				continue
			}
			seen[role.Key] = true
			existing, ok := byKey[role.Key]
			same := ok && existing.Label == role.Label && existing.Emoji == role.Emoji && existing.Body == role.Body && existing.Cardinality == role.Cardinality &&
				existing.Lead == role.Lead && existing.CanSpawn == role.CanSpawn && existing.Writer == role.Writer
			switch {
			case !ok:
				sections["roles"].New++
				note("roles", "new", role.Key, hashOf(role), "absent")
			case same:
				sections["roles"].Unchanged++
				continue
			default:
				sections["roles"].Overwrite++
				note("roles", "overwrite", role.Key, hashOf(role), hashOf(existing))
			}
			steps = append(steps, role)
		}
		if len(steps) > 0 {
			p.run["roles"] = func(ctx context.Context) (int, []backupFailure) {
				var failed []backupFailure
				applied := 0
				for _, role := range steps {
					definition := store.RoleDefinition{Key: role.Key, Label: role.Label, Emoji: role.Emoji, Body: role.Body, Cardinality: role.Cardinality,
						Lead: role.Lead, CanSpawn: role.CanSpawn, Writer: role.Writer}
					if existing, ok := byKey[role.Key]; ok {
						definition.ID, definition.Rev = existing.ID, existing.Rev
					}
					if _, err := s.Store.ManageRole(ctx, user, definition, false); err != nil {
						failed = append(failed, backupFailure{Section: "roles", Key: role.Key, Error: backupErrorText(err)})
					} else {
						applied++
					}
				}
				return applied, failed
			}
		}
	}

	// UI preferences: checked for shape; the browser applies them.
	for _, name := range sortedKeys(b.UIPrefs) {
		value := b.UIPrefs[name]
		switch {
		case !backupUIPrefNames[name]:
			sections["ui_prefs"].Invalid = append(sections["ui_prefs"].Invalid, backupIssue{Key: name, Reason: "not a known preference"})
		case len(value) > 256:
			sections["ui_prefs"].Invalid = append(sections["ui_prefs"].Invalid, backupIssue{Key: name, Reason: "the value is too long"})
		default:
			sections["ui_prefs"].Overwrite++
		}
	}

	for _, name := range backupSections {
		section := sections[name]
		if section.New+section.Overwrite+section.Unchanged+len(section.Invalid) == 0 && section.Unavailable == "" {
			continue
		}
		p.preview.Sections = append(p.preview.Sections, *section)
	}
	sort.Strings(p.lines)
	sum := sha256.Sum256([]byte(strings.Join(p.lines, "\n")))
	p.preview.Digest = hex.EncodeToString(sum[:])
	return p, nil
}

func dropSection(lines []string, section string) []string {
	kept := lines[:0:0]
	for _, line := range lines {
		if !strings.HasPrefix(line, section+"\x00") {
			kept = append(kept, line)
		}
	}
	return kept
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func backupErrorText(err error) string {
	switch {
	case errors.Is(err, store.ErrForbidden):
		return "insufficient permission"
	case errors.Is(err, store.ErrStaleRev):
		return "changed while importing; preview again"
	case errors.Is(err, store.ErrConflict):
		return "name or key already exists"
	case errors.Is(err, store.ErrRoleNameCollision):
		return "the role key collides with an agent slug or a person's login"
	case errors.Is(err, store.ErrRoleInUse):
		return "agents are running under this role; stop them first"
	case errors.Is(err, store.ErrInvalidTransition):
		return "not a valid definition"
	default:
		return err.Error()
	}
}

type backupRequest struct {
	Backup  settingsBackup
	Ignored []string
	Digest  string
	// Sections is the chosen subset; nil means the field was absent, which applies every section that has changes. A field that is
	// present and empty chooses nothing.
	Sections *[]string
}

// readSettingsBackup reads and checks the file's identity; any failure has already been answered.
func (s *Server) readSettingsBackup(w http.ResponseWriter, r *http.Request) (backupRequest, bool) {
	var raw struct {
		Backup   json.RawMessage `json:"backup"`
		Digest   string          `json:"digest"`
		Sections *[]string       `json:"sections"`
	}
	if !readJSONLimit(w, r, &raw, settingsBackupLimit) {
		return backupRequest{}, false
	}
	body := backupRequest{Digest: raw.Digest, Sections: raw.Sections}
	if len(raw.Backup) == 0 || json.Unmarshal(raw.Backup, &body.Backup) != nil {
		apiError(w, 400, "invalid_request", "backup is required and must be a settings backup object")
		return body, false
	}
	var members map[string]json.RawMessage
	_ = json.Unmarshal(raw.Backup, &members)
	known := map[string]bool{"schema": true, "version": true, "exported_at": true}
	for _, name := range backupSections {
		known[name] = true
	}
	for _, name := range sortedKeys(members) {
		if !known[name] {
			body.Ignored = append(body.Ignored, name)
		}
	}
	if body.Backup.Schema != settingsBackupSchema {
		apiError(w, 400, "invalid_request", "this is not an aimebu settings backup")
		return body, false
	}
	if body.Backup.Version != settingsBackupVersion {
		apiError(w, 400, "unsupported_version", fmt.Sprintf("settings backup version %d is not supported (this server reads version %d)", body.Backup.Version, settingsBackupVersion))
		return body, false
	}
	return body, true
}

// GET /api/settings-backup: the owner's settings as a downloadable file.
func (s *Server) settingsBackupExport(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	b, err := s.exportSettingsBackup(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="aimebu-settings-backup.json"`)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, b)
}

// POST /api/settings-backup/preview: what importing the file would change; nothing is saved.
func (s *Server) settingsBackupPreview(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	request, ok := s.readSettingsBackup(w, r)
	if !ok {
		return
	}
	plan, err := s.planSettingsBackup(r.Context(), userID(r), request.Backup)
	if err != nil {
		fail(w, err)
		return
	}
	plan.preview.Ignored = request.Ignored
	writeJSON(w, 200, plan.preview)
}

// POST /api/settings-backup/apply: applies a previewed import. The digest must be the one the preview answered, so a
// change to the instance since then, or a different file, is refused. Sections apply independently, in a fixed order;
// within a section an entry that cannot be saved is reported and the rest still apply.
func (s *Server) settingsBackupApply(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	request, ok := s.readSettingsBackup(w, r)
	if !ok {
		return
	}
	plan, err := s.planSettingsBackup(r.Context(), userID(r), request.Backup)
	if err != nil {
		fail(w, err)
		return
	}
	if request.Digest == "" || request.Digest != plan.preview.Digest {
		apiError(w, 409, "preview_stale", "the settings changed since the preview, or the file is different; preview it again")
		return
	}
	only := map[string]bool{}
	restrict := request.Sections != nil
	if restrict {
		for _, name := range *request.Sections {
			only[name] = true
		}
	}
	result := backupResult{Complete: true}
	for _, name := range backupSections {
		run := plan.run[name]
		if run == nil || (restrict && !only[name]) {
			continue
		}
		applied, failed := run(r.Context())
		result.Sections = append(result.Sections, backupSectionResult{Section: name, Applied: applied})
		if len(failed) > 0 {
			result.Failed = append(result.Failed, failed...)
			result.Complete = false
		}
	}
	writeJSON(w, 200, result)
}
