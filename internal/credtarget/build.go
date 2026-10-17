package credtarget

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// BuildTarget returns the new content of a target file: existing (may be nil
// when the file is missing) merged with the token fields of full (a store
// credential). It is a read-modify-write, never a replacement (design B2): every
// member the server does not own is written back byte for byte.
func BuildTarget(tool string, existing, full []byte) ([]byte, error) {
	switch tool {
	case ToolClaude:
		return BuildClaudeTarget(existing, full)
	case ToolCodex:
		return BuildCodexTarget(existing, full)
	}
	return nil, fmt.Errorf("credtarget: unknown tool %q", tool)
}

// BuildFullTarget restores the refresh token while preserving unrelated live
// fields such as Claude's mcpOAuth. The caller must have verified that full is
// the current stored credential and that no new login is pending in existing.
func BuildFullTarget(tool string, existing, full []byte) ([]byte, error) {
	built, err := BuildTarget(tool, existing, full)
	if err != nil {
		return nil, err
	}
	switch tool {
	case ToolClaude:
		fields, err := claudeRefreshFields(full)
		if err != nil {
			return nil, err
		}
		doc, section, _, err := openDocument(built, claudeSection, "")
		if err != nil {
			return nil, err
		}
		for key, value := range fields {
			section.set(key, value)
		}
		doc.set(claudeSection, section.bytes())
		return doc.bytes(), nil
	case ToolCodex:
		doc, section, _, err := openDocument(built, codexTokens, "")
		if err != nil {
			return nil, err
		}
		source, err := parseObject(full)
		if err != nil {
			return nil, err
		}
		raw, ok := source.value(codexTokens)
		if !ok {
			return nil, errors.New("credtarget: codex credential has no tokens")
		}
		tokens, err := parseObject(raw)
		if err != nil {
			return nil, err
		}
		refresh, ok := tokens.value(codexRefreshKey)
		if !ok || !nonEmptyString(refresh) {
			return nil, errors.New("credtarget: codex credential has no refresh token")
		}
		section.set(codexRefreshKey, refresh)
		doc.set(codexTokens, section.bytes())
		return doc.bytes(), nil
	}
	return nil, fmt.Errorf("credtarget: unknown tool %q", tool)
}

func claudeRefreshFields(full []byte) (map[string]json.RawMessage, error) {
	doc, err := parseObject(full)
	if err != nil {
		return nil, err
	}
	raw, ok := doc.value(claudeSection)
	if !ok {
		raw, ok = doc.value(claudeSectionLegacy)
	}
	if !ok {
		return nil, errors.New("credtarget: claude credential has no OAuth section")
	}
	section, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	for _, key := range claudeRefreshKeys {
		if value, ok := section.value(key); ok && nonEmptyString(value) {
			return map[string]json.RawMessage{"refreshToken": value}, nil
		}
	}
	return nil, errors.New("credtarget: claude credential has no refresh token")
}

// Claude keeps the login in the `claudeAiOauth` object of .credentials.json;
// an older spelling, `claude_ai_oauth` with snake_case fields, is still read.
const (
	claudeSection       = "claudeAiOauth"
	claudeSectionLegacy = "claude_ai_oauth"
)

// claudeManaged are the fields of the section that describe the login itself.
// They follow the full credential: a field it lacks is removed from the target,
// since a stale value would belong to the previous account.
var claudeManaged = []string{"accessToken", "expiresAt", "scopes", "subscriptionType", "rateLimitTier", "refreshTokenExpiresAt"}

var claudeAliases = map[string]string{
	"access_token":             "accessToken",
	"expires_at":               "expiresAt",
	"subscription_type":        "subscriptionType",
	"rate_limit_tier":          "rateLimitTier",
	"refresh_token_expires_at": "refreshTokenExpiresAt",
}

var claudeRefreshKeys = []string{"refreshToken", "refresh_token"}

// BuildClaudeTarget replaces ONLY the `claudeAiOauth` token fields of existing
// (accessToken, expiresAt, scopes, subscriptionType, rateLimitTier,
// refreshTokenExpiresAt) with full's, DELETES the `refreshToken` key (never ""),
// and keeps every other key untouched (mcpOAuth, gateway pin, ...). With no
// existing file it creates a document holding only `claudeAiOauth`.
//
// Callers must have rescued a refresh token that existing might hold (a /login
// that landed in the target) before they write the result; see Writer.
func BuildClaudeTarget(existing, full []byte) ([]byte, error) {
	fields, err := claudeFields(full)
	if err != nil {
		return nil, err
	}
	doc, base, legacy, err := openDocument(existing, claudeSection, claudeSectionLegacy)
	if err != nil {
		return nil, err
	}
	section := base
	if section == nil {
		section = emptyObject(doc)
	}
	for _, name := range claudeManaged {
		if raw, ok := fields[name]; ok {
			section.set(name, raw)
		} else {
			section.delete(name)
		}
	}
	for _, name := range orderedKeys(fields) {
		if !contains(claudeManaged, name) {
			section.set(name, fields[name])
		}
	}
	for _, key := range claudeRefreshKeys {
		section.delete(key)
	}
	doc.set(claudeSection, section.bytes())
	if legacy {
		doc.delete(claudeSectionLegacy)
	}
	return doc.bytes(), nil
}

// claudeFields reads the login fields out of a full credential, normalised to
// the camelCase names Claude uses, without any refresh token.
func claudeFields(full []byte) (map[string]json.RawMessage, error) {
	doc, err := parseObject(full)
	if err != nil {
		return nil, fmt.Errorf("credtarget: claude credential: %w", err)
	}
	raw, ok := doc.value(claudeSection)
	if !ok {
		raw, ok = doc.value(claudeSectionLegacy)
	}
	if !ok {
		return nil, errors.New("credtarget: claude credential has no OAuth section")
	}
	section, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("credtarget: claude OAuth section: %w", err)
	}
	fields := map[string]json.RawMessage{}
	for _, m := range section.members {
		name := m.name
		if alias, ok := claudeAliases[name]; ok {
			name = alias
		}
		if contains(claudeRefreshKeys, name) {
			continue
		}
		fields[name] = compactValue(m.value)
	}
	if !nonEmptyString(fields["accessToken"]) {
		return nil, errors.New("credtarget: claude credential has no access token")
	}
	return fields, nil
}

// Codex keeps its login in `tokens`, with the mode in `auth_mode`.
const (
	codexMode        = "chatgptAuthTokens"
	codexTokens      = "tokens"
	codexRefreshKey  = "refresh_token"
	codexLastRefresh = "last_refresh"
)

var codexManaged = []string{"id_token", "access_token", "account_id"}

var codexAliases = map[string]string{"idToken": "id_token", "accessToken": "access_token", "accountId": "account_id", "refreshToken": codexRefreshKey}

// BuildCodexTarget replaces `tokens.{id_token,access_token,account_id}` and
// `last_refresh`, forces `auth_mode` to "chatgptAuthTokens" and
// `tokens.refresh_token` to "" (key PRESENT: deleting it logs codex out), and
// keeps every other key (OPENAI_API_KEY, ...).
func BuildCodexTarget(existing, full []byte) ([]byte, error) {
	fields, lastRefresh, err := codexFields(full)
	if err != nil {
		return nil, err
	}
	doc, base, _, err := openDocument(existing, codexTokens, "")
	if err != nil {
		return nil, err
	}
	tokens := base
	if tokens == nil {
		tokens = emptyObject(doc)
	}
	for alias := range codexAliases {
		tokens.delete(alias)
	}
	for _, name := range codexManaged {
		if raw, ok := fields[name]; ok {
			tokens.set(name, raw)
		} else {
			tokens.delete(name)
		}
	}
	tokens.set(codexRefreshKey, []byte(`""`))
	doc.set("auth_mode", []byte(`"`+codexMode+`"`))
	doc.set(codexTokens, tokens.bytes())
	if lastRefresh != nil {
		doc.set(codexLastRefresh, lastRefresh)
	} else {
		doc.delete(codexLastRefresh)
	}
	return doc.bytes(), nil
}

func codexFields(full []byte) (fields map[string]json.RawMessage, lastRefresh json.RawMessage, err error) {
	doc, err := parseObject(full)
	if err != nil {
		return nil, nil, fmt.Errorf("credtarget: codex credential: %w", err)
	}
	raw, ok := doc.value(codexTokens)
	if !ok {
		return nil, nil, errors.New("credtarget: codex credential has no tokens")
	}
	section, err := parseObject(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("credtarget: codex tokens: %w", err)
	}
	fields = map[string]json.RawMessage{}
	for _, m := range section.members {
		name := m.name
		if alias, ok := codexAliases[name]; ok {
			name = alias
		}
		if name == codexRefreshKey {
			continue
		}
		fields[name] = compactValue(m.value)
	}
	if !nonEmptyString(fields["access_token"]) {
		return nil, nil, errors.New("credtarget: codex credential has no access token")
	}
	if v, ok := doc.value(codexLastRefresh); ok && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		lastRefresh = compactValue(v)
	}
	return fields, lastRefresh, nil
}

// StripRefreshToken returns data with its refresh token removed in the target
// form of the tool (claude: key deleted; codex: ""), preserving everything
// else. Used by the reconciler after a login import.
func StripRefreshToken(tool string, data []byte) ([]byte, error) {
	switch tool {
	case ToolClaude:
		doc, err := parseObject(data)
		if err != nil {
			return nil, fmt.Errorf("credtarget: claude credential: %w", err)
		}
		for _, name := range []string{claudeSection, claudeSectionLegacy} {
			raw, ok := doc.value(name)
			if !ok {
				continue
			}
			section, err := parseObject(raw)
			if err != nil {
				return nil, fmt.Errorf("credtarget: claude OAuth section: %w", err)
			}
			for _, key := range claudeRefreshKeys {
				section.delete(key)
			}
			doc.set(name, section.bytes())
		}
		return doc.bytes(), nil
	case ToolCodex:
		doc, err := parseObject(data)
		if err != nil {
			return nil, fmt.Errorf("credtarget: codex credential: %w", err)
		}
		raw, ok := doc.value(codexTokens)
		if !ok {
			return doc.bytes(), nil
		}
		tokens, err := parseObject(raw)
		if err != nil {
			return nil, fmt.Errorf("credtarget: codex tokens: %w", err)
		}
		tokens.delete("refreshToken")
		tokens.set(codexRefreshKey, []byte(`""`))
		doc.set(codexTokens, tokens.bytes())
		return doc.bytes(), nil
	}
	return nil, fmt.Errorf("credtarget: unknown tool %q", tool)
}

// openDocument parses an existing target (nil or empty means a new document)
// and returns it together with its login section: the member called section, or
// failing that legacy. hasLegacy reports that the old spelling is present, so
// the caller can drop it after it has written the section under the new name.
func openDocument(existing []byte, section, legacy string) (doc, base *object, hasLegacy bool, err error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		doc, _ = parseObject([]byte("{\n}"))
		return doc, nil, false, nil
	}
	doc, err = parseObject(existing)
	if err != nil {
		return nil, nil, false, fmt.Errorf("credtarget: existing credential file: %w", err)
	}
	_, found := doc.value(legacy)
	hasLegacy = legacy != "" && found
	names := []string{section}
	if legacy != "" {
		names = append(names, legacy)
	}
	for _, name := range names {
		raw, ok := doc.value(name)
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		base, err = parseObject(raw)
		if err != nil {
			return nil, nil, false, fmt.Errorf("credtarget: existing %q: %w", name, err)
		}
		return doc, base, hasLegacy, nil
	}
	return doc, nil, hasLegacy, nil
}

// emptyObject returns an empty object laid out like doc's members: multi-line
// with the closing brace indented to the member's level when the document spans
// several lines, compact otherwise.
func emptyObject(doc *object) *object {
	if !bytes.Contains(doc.bytes(), []byte("\n")) {
		o, _ := parseObject([]byte("{}"))
		return o
	}
	indent := memberIndent(doc)
	o, _ := parseObject([]byte("{\n" + indent + "}"))
	return o
}

// memberIndent is the whitespace that precedes the members of doc on their
// line, two spaces when it cannot be told.
func memberIndent(doc *object) string {
	for _, m := range doc.members {
		before, _ := splitHead(m)
		if nl := bytes.LastIndexByte(before, '\n'); nl >= 0 {
			return string(before[nl+1:])
		}
	}
	return "  "
}

func compactValue(raw []byte) []byte {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return raw
	}
	return out.Bytes()
}

func nonEmptyString(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value != ""
}

func contains(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

func orderedKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	// Stable output regardless of map order.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
