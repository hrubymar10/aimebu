package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

const (
	ollamaSettingsURL = "https://ollama.com/settings"
	ollamaBalanceURL  = "https://ollama.com/api/balance"
	// This tracks a currently shipping Chrome-like user agent for the settings page.
	ollamaUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	ollamaAccept    = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

	OllamaAuthAuto   = "auto"
	OllamaAuthCookie = "cookie"
	OllamaAuthAPIKey = "api_key"

	ollamaSessionWindowSeconds int64 = 5 * 3600
	ollamaWeeklyWindowSeconds  int64 = 7 * 24 * 3600
	ollamaMonthlyWindowSeconds int64 = 30 * 24 * 3600
)

var (
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaPlanAnchor = regexp.MustCompile(`(?is)Cloud Usage\s*</span\s*>\s*<span[^>]*>([^<]+)</span\s*>`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaWalletPlanAnchor = regexp.MustCompile(`(?is)<h[1-6][^>]*>\s*Usage credits\s*<span[^>]*>\s*([^<]+)\s*</span\s*>`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaEmailAnchor = regexp.MustCompile(`(?is)id=["']header-email["'][^>]*>([^<]+)<`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaSessionAnchor = "Session usage"
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaHourlyAnchor = "Hourly usage"
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaWeeklyAnchor = "Weekly usage"
	// Free plans label the included-credit meter "Free usage" instead of "Monthly usage"; both are the monthly window.
	ollamaFreeAnchor    = "Free usage"
	ollamaMonthlyAnchor = "Monthly usage"
	ollamaUsageAnchors  = []string{ollamaSessionAnchor, ollamaHourlyAnchor, ollamaWeeklyAnchor, ollamaMonthlyAnchor, ollamaFreeAnchor}
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaPercentAnchor = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*%\s*used`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaWidthPercentAnchor = regexp.MustCompile(`(?i)width:\s*([0-9]+(?:\.[0-9]+)?)%`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaResetAnchor = regexp.MustCompile(`data-time=["']([^"']+)["']`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaSignedOutAnchor = regexp.MustCompile(`(?is)(sign in to ollama|log in to ollama|/auth/login|/auth/signin|/signin)`)

	// Usage labels match their own text node (`>Label<`), never a bare substring: explanatory
	// prose such as "Free usage credits can be used with the following cloud models:" contains
	// a usage label, which would both start a window before the real meter and truncate the
	// previous block's reset timestamp at a spurious boundary.
	ollamaLabelTextNodes = ollamaBuildLabelTextNodes(ollamaUsageAnchors)

	// Credit-wallet fields are matched by complete elements within their wallet section, so a
	// missing value cannot borrow adjacent spending or prose.
	ollamaWalletHeadingAnchor = regexp.MustCompile(`(?is)<h[1-6][^>]*>\s*Usage credits(?:\s*<span[^>]*>[^<]*</span\s*>)?\s*</h[1-6]\s*>`)
	ollamaWalletSectionEnd    = regexp.MustCompile(`(?is)</section\s*>|<h[1-6][^>]*>`)
	ollamaWalletAmountPattern = `(\$(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.[0-9]+)?)`
	ollamaWalletBalanceAnchor = regexp.MustCompile(`(?i)^\s*(?:<div[^>]*>\s*)*<span[^>]*>\s*` + ollamaWalletAmountPattern + `\s*</span\s*>`)
	ollamaWalletUsedAnchor    = regexp.MustCompile(`(?i)<span[^>]*>\s*Monthly credits used\s*</span\s*>\s*<span[^>]*>\s*` + ollamaWalletAmountPattern + `\s*</span\s*>`)

	ollamaHTTPClient = newUsageHTTPClient()
)

// ollamaBuildLabelTextNodes compiles one `>Label<` matcher per usage label.
func ollamaBuildLabelTextNodes(labels []string) map[string]*regexp.Regexp {
	patterns := make(map[string]*regexp.Regexp, len(labels))
	for _, label := range labels {
		patterns[label] = regexp.MustCompile(`(?is)>\s*` + regexp.QuoteMeta(label) + `\s*<`)
	}
	return patterns
}

func ollamaLabelTextNode(label string) *regexp.Regexp {
	if pattern, ok := ollamaLabelTextNodes[label]; ok {
		return pattern
	}
	return regexp.MustCompile(`(?is)>\s*` + regexp.QuoteMeta(label) + `\s*<`)
}

type ollamaCloudProvider struct{}

func NewOllamaCloudProvider() UsageProvider { return ollamaCloudProvider{} }

func (ollamaCloudProvider) Key() string { return ProviderOllamaCloud }

func (ollamaCloudProvider) Fetch(ctx context.Context, store *Store) (Snapshot, error) {
	cfg, err := store.LoadConfig()
	if err != nil {
		return ollamaStatus(StatusFetchError, "Ollama Cloud usage config could not be read.", nil), nil
	}
	pc := cfg.Providers[ProviderOllamaCloud]
	authMode := normalizeOllamaAuthMode(pc.AuthMode)
	cookie := strings.TrimSpace(pc.Cookie)
	apiKey := normalizeOllamaAPIKey(pc.APIKey)
	switch authMode {
	case OllamaAuthCookie:
		return fetchOllamaWithCookie(ctx, cookie)
	case OllamaAuthAPIKey:
		return fetchOllamaWithAPIKey(ctx, apiKey)
	default:
		if cookie != "" {
			snap, err := fetchOllamaWithCookie(ctx, cookie)
			if err == nil && snap.Status == StatusOK {
				return snap, nil
			}
			if apiKey == "" {
				return snap, err
			}
		}
		if apiKey != "" {
			return fetchOllamaWithAPIKey(ctx, apiKey)
		}
		return ollamaStatus(StatusAuthMissing, "Configure an Ollama Cloud cookie or select API key mode and add a key.", fieldDetail("config.ollama-cloud.auth", "missing")), nil
	}
}

func fetchOllamaWithCookie(ctx context.Context, cookie string) (Snapshot, error) {
	if cookie == "" {
		return ollamaStatus(StatusAuthMissing, "Ollama Cloud cookie is not configured. Paste the Cookie request header from Ollama Settings or select API key mode.", fieldDetail("config.ollama-cloud.cookie", "missing")), nil
	}
	normalized, detail, err := normalizeOllamaCookieHeader(cookie)
	if err != nil {
		return ollamaStatus(StatusAuthMissing, "The saved cookie has no recognized session cookie. Paste the full Cookie request header from Ollama Settings or select API key mode.", detail), nil
	}
	var signedOutErr error
	var signedOutDetail *ErrorDetail
	for _, candidate := range ollamaCookieCandidates(normalized) {
		body, detail, status, err := fetchOllamaSettingsHTML(ctx, candidate)
		if err != nil {
			snap := ollamaStatus(status, err.Error(), detail)
			if status == StatusFetchError || status == StatusTimeout {
				return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
			}
			return snap, nil
		}
		snap, detail, err := parseOllamaSettingsHTML(body)
		if err != nil {
			if detail != nil && detail.Fields["page"] == "signed_out" {
				signedOutErr = err
				signedOutDetail = detail
				continue
			}
			snap = ollamaStatus(StatusFetchError, err.Error(), detail)
			return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
		}
		if detail != nil && len(detail.Fields) > 0 {
			snap.ErrorDetail = detail
		}
		return snap, nil
	}
	if signedOutErr != nil {
		return ollamaStatus(StatusAuthMissing, "Ollama Cloud rejected the cookie. Sign in again and paste a fresh Cookie request header, or select API key mode.", signedOutDetail), nil
	}
	return ollamaStatus(StatusFetchError, "Ollama Cloud settings page did not return usage data.", nil), nil
}

func fetchOllamaWithAPIKey(ctx context.Context, apiKey string) (Snapshot, error) {
	if apiKey == "" {
		return ollamaStatus(StatusAuthMissing, "Ollama Cloud API key is not configured. Add a key or select cookie mode.", fieldDetail("config.ollama-cloud.api_key", "missing")), nil
	}
	data, detail, status, err := fetchOllamaBalance(ctx, apiKey)
	if err != nil {
		snap := ollamaStatus(status, err.Error(), detail)
		if status == StatusFetchError || status == StatusTimeout {
			return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
		}
		return snap, nil
	}
	snap, detail, err := normalizeOllamaBalance(data)
	if err != nil {
		snap := ollamaStatus(StatusFetchError, err.Error(), detail)
		return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
	}
	if detail != nil && len(detail.Fields) > 0 {
		snap.ErrorDetail = detail
	}
	return snap, nil
}

func normalizeOllamaAuthMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case OllamaAuthCookie, "web":
		return OllamaAuthCookie
	case OllamaAuthAPIKey, "api":
		return OllamaAuthAPIKey
	default:
		return OllamaAuthAuto
	}
}

func normalizeOllamaAPIKey(input string) string {
	value := strings.TrimSpace(input)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	return strings.TrimSpace(value)
}

func parseOllamaCookieInput(input string) (map[string]string, error) {
	normalized, _, err := normalizeOllamaCookieHeader(input)
	if err != nil {
		return nil, err
	}
	pairs := map[string]string{}
	for _, part := range strings.Split(normalized, ";") {
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name != "" {
			pairs[name] = value
		}
	}
	return pairs, nil
}

func normalizeOllamaCookieHeader(input string) (string, *ErrorDetail, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", fieldDetail("cookie", "missing"), errors.New("Ollama Cloud cookie is empty.")
	}
	if extracted := extractOllamaCookieHeader(value); extracted != "" {
		value = extracted
	}
	value = strings.TrimSpace(stripWrappingCookieQuotes(value))
	if strings.HasPrefix(strings.ToLower(value), "cookie:") {
		value = strings.TrimSpace(value[len("cookie:"):])
	}
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")

	pairs := map[string]string{}
	for _, part := range strings.Split(value, ";") {
		name, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		val = strings.TrimSpace(stripWrappingCookieQuotes(val))
		if name != "" {
			pairs[name] = val
		}
	}
	if len(pairs) == 0 {
		return "", fieldDetail("cookie", "missing"), errors.New("Ollama Cloud cookie did not contain cookie pairs.")
	}
	hasSession := false
	for name := range pairs {
		if isOllamaSessionCookieName(name) {
			hasSession = true
			break
		}
	}
	if !hasSession {
		return "", fieldDetail("cookie", "no_session_cookie"), errors.New("Ollama Cloud cookie is missing a recognized session cookie.")
	}
	return serializeOllamaCookies(pairs), nil, nil
}

func extractOllamaCookieHeader(input string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?is)-H\s*'Cookie:\s*([^']+)'`),
		regexp.MustCompile(`(?is)-H\s*"Cookie:\s*([^"]+)"`),
		regexp.MustCompile(`(?im)^\s*cookie:\s*['"]([^'"]+)['"]\s*$`),
		regexp.MustCompile(`(?im)^\s*cookie:\s*'?([^'"\r\n]+)'?\s*$`),
		regexp.MustCompile(`(?is)(?:^|\s)(?:--cookie|-b)\s*'([^']+)'`),
		regexp.MustCompile(`(?is)(?:^|\s)(?:--cookie|-b)\s*"([^"]+)"`),
		regexp.MustCompile(`(?is)(?:^|\s)(?:--cookie|-b)\s+([^\s]+=[^\s]+(?:\s*;\s*[^\s=]+=[^\s]+)*)`),
	}
	for _, pattern := range patterns {
		matches := pattern.FindStringSubmatch(input)
		if len(matches) > 1 && strings.TrimSpace(matches[1]) != "" {
			return strings.TrimSpace(matches[1])
		}
	}
	return ""
}

func stripWrappingCookieQuotes(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func isOllamaSessionCookieName(name string) bool {
	switch name {
	case "session", "__Secure-session", "ollama_session", "__Host-ollama_session", "wos-session", "__Secure-next-auth.session-token", "next-auth.session-token":
		return true
	}
	return strings.HasPrefix(name, "__Secure-next-auth.session-token.") ||
		strings.HasPrefix(name, "next-auth.session-token.")
}

func serializeOllamaCookies(pairs map[string]string) string {
	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+pairs[key])
	}
	return strings.Join(parts, "; ")
}

func ollamaCookieCandidates(normalized string) []string {
	pairs := map[string]string{}
	for _, part := range strings.Split(normalized, ";") {
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name != "" {
			pairs[name] = value
		}
	}
	candidates := []string{normalized}
	seen := map[string]bool{normalized: true}
	add := func(candidate string) {
		if candidate == "" || seen[candidate] {
			return
		}
		seen[candidate] = true
		candidates = append(candidates, candidate)
	}

	keys := make([]string, 0, len(pairs))
	for name := range pairs {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		value := pairs[name]
		if isOllamaSessionCookieName(name) && !strings.Contains(name, ".session-token.") {
			add(serializeOllamaCookies(map[string]string{name: value}))
		}
	}
	for _, base := range []string{"__Secure-next-auth.session-token", "next-auth.session-token"} {
		chunked := map[string]string{}
		if value, ok := pairs[base]; ok {
			chunked[base] = value
		}
		for name, value := range pairs {
			if strings.HasPrefix(name, base+".") {
				chunked[name] = value
			}
		}
		if len(chunked) > 0 {
			add(serializeOllamaCookies(chunked))
		}
	}
	return candidates
}

func fetchOllamaSettingsHTML(ctx context.Context, cookie string) ([]byte, *ErrorDetail, Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaSettingsURL, nil)
	if err != nil {
		return nil, nil, StatusFetchError, err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", ollamaUserAgent)
	req.Header.Set("Accept", ollamaAccept)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://ollama.com")
	req.Header.Set("Referer", ollamaSettingsURL)
	resp, err := doWithRetry(ctx, ollamaHTTPClient, req, RetryPolicy{MaxRetries: 1})
	if err != nil {
		return nil, nil, usageRequestStatus(err), fmt.Errorf("Ollama Cloud settings request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK && isOllamaSignInRedirect(responseFinalURL(resp)):
		return nil, fieldDetail("settings", "signin_redirect"), StatusAuthMissing, errors.New("Ollama Cloud settings page requires sign in.")
	case resp.StatusCode == http.StatusOK && isUsageCrossOriginRedirect(resp, ollamaSettingsURL):
		return nil, fieldDetail("settings", "cross_origin_redirect"), StatusAuthMissing, errors.New("Ollama Cloud settings page requires sign in.")
	case resp.StatusCode == http.StatusOK:
		return data, nil, "", nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fieldDetail("settings", "auth"), StatusAuthMissing, errors.New("Ollama Cloud rejected the cookie. Sign in again and paste a fresh Cookie request header, or select API key mode.")
	default:
		return nil, fieldDetail("settings", fmt.Sprintf("http_%d", resp.StatusCode)), StatusFetchError, fmt.Errorf("Ollama Cloud settings endpoint returned HTTP %d.", resp.StatusCode)
	}
}

func fetchOllamaBalance(ctx context.Context, apiKey string) ([]byte, *ErrorDetail, Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaBalanceURL, nil)
	if err != nil {
		return nil, nil, StatusFetchError, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aimebu")
	resp, err := doWithRetry(ctx, ollamaHTTPClient, req, RetryPolicy{MaxRetries: 1})
	if err != nil {
		return nil, nil, usageRequestStatus(err), fmt.Errorf("Ollama Cloud API request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK && isOllamaSignInRedirect(responseFinalURL(resp)):
		return nil, fieldDetail("balance", "signin_redirect"), StatusAuthMissing, errors.New("Ollama Cloud API key was rejected or revoked.")
	case resp.StatusCode == http.StatusOK && isUsageCrossOriginRedirect(resp, ollamaBalanceURL):
		return nil, fieldDetail("balance", "cross_origin_redirect"), StatusAuthMissing, errors.New("Ollama Cloud API key was rejected or revoked.")
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, jsonShapeDetail("balance", data), StatusAuthMissing, errors.New("Ollama Cloud rejected the API key. Create a fresh key or select cookie mode.")
	default:
		return nil, httpStatusDetail("balance", data, resp.StatusCode), StatusFetchError, fmt.Errorf("Ollama Cloud balance endpoint returned HTTP %d.", resp.StatusCode)
	}
	return data, nil, "", nil
}

// ollamaBalanceRaw keeps the flexible amounts as raw JSON: numeric and decimal-string
// amounts are both accepted, and anything else is a parse failure rather than a zero.
type ollamaBalanceRaw struct {
	Included  json.RawMessage `json:"included"`
	Purchased json.RawMessage `json:"purchased"`
}

func normalizeOllamaBalance(data []byte) (Snapshot, *ErrorDetail, error) {
	unrecognized := func() (Snapshot, *ErrorDetail, error) {
		return Snapshot{}, jsonShapeDetail("balance", data), errors.New("Ollama returned an unrecognized credit balance response.")
	}
	var raw ollamaBalanceRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return unrecognized()
	}
	snap := Snapshot{Status: StatusOK, Plan: "API key verified"}
	sawIncluded := false
	if len(ollamaTrimNull(raw.Purchased)) > 0 {
		purchased, ok := ollamaBalanceObject(raw.Purchased)
		if !ok {
			return unrecognized()
		}
		balance, ok := ollamaBalanceAmount(purchased, "balance_usd")
		if !ok {
			return unrecognized()
		}
		snap.Credits = &Credits{Label: "Credit balance", Balance: balance}
	}
	if len(ollamaTrimNull(raw.Included)) > 0 {
		fields, ok := ollamaBalanceObject(raw.Included)
		if !ok {
			return unrecognized()
		}
		sawIncluded = true
		session, hasSession := ollamaBalanceField(fields, "session")
		weekly, hasWeekly := ollamaBalanceField(fields, "weekly")
		if hasSession || hasWeekly {
			// Legacy shape: per-window remaining percentages instead of a credit allowance.
			if hasSession {
				window, ok, err := ollamaLegacyBalanceWindow(session, "session", ollamaSessionWindowSeconds)
				if err != nil {
					return unrecognized()
				}
				if ok {
					snap.Windows = append(snap.Windows, window)
				}
			}
			if hasWeekly {
				window, ok, err := ollamaLegacyBalanceWindow(weekly, "weekly", ollamaWeeklyWindowSeconds)
				if err != nil {
					return unrecognized()
				}
				if ok {
					snap.Windows = append(snap.Windows, window)
				}
			}
		} else {
			allowance, ok := ollamaBalanceAmount(fields, "allowance_usd")
			if !ok || allowance < 0 {
				return unrecognized()
			}
			balance, ok := ollamaBalanceAmount(fields, "balance_usd")
			if !ok {
				return unrecognized()
			}
			period, hasPeriod := ollamaBalanceField(fields, "period")
			var reset *time.Time
			if hasPeriod {
				var periodFields map[string]json.RawMessage
				if err := json.Unmarshal(period, &periodFields); err != nil {
					return unrecognized()
				}
				until, hasUntil := ollamaBalanceField(periodFields, "until")
				if hasUntil {
					t, ok := ollamaBalanceTimestamp(until)
					if !ok {
						return unrecognized()
					}
					reset = &t
				}
			}
			// A zero allowance carries no percentage; the purchased credits stay the only balance.
			if allowance > 0 {
				used := allowance - balance
				if used < 0 {
					used = 0
				}
				window := normalizedWindow("monthly", used/allowance*100, reset, ollamaMonthlyWindowSeconds)
				window.Pace = computeWindowPace(window, time.Now())
				snap.Windows = append(snap.Windows, window)
			}
		}
	}
	if len(snap.Windows) == 0 && snap.Credits == nil && !sawIncluded {
		return unrecognized()
	}
	return snap, nil, nil
}

// ollamaBalanceObject decodes a present, non-null JSON object field.
func ollamaBalanceObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	raw = ollamaTrimNull(raw)
	if len(raw) == 0 {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false
	}
	return object, true
}

func ollamaBalanceField(fields map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, ok := fields[key]
	return raw, ok && len(ollamaTrimNull(raw)) > 0
}

func ollamaTrimNull(raw json.RawMessage) json.RawMessage {
	trimmed := json.RawMessage(strings.TrimSpace(string(raw)))
	if string(trimmed) == "null" {
		return nil
	}
	return trimmed
}

// ollamaBalanceAmount accepts a JSON number or a decimal-string amount and bounds it
// to a sane magnitude; every other shape is unusable.
func ollamaBalanceAmount(fields map[string]json.RawMessage, key string) (float64, bool) {
	raw, ok := ollamaBalanceField(fields, key)
	if !ok {
		return 0, false
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, validOllamaBalanceAmount(number)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, false
	}
	return number, validOllamaBalanceAmount(number)
}

func validOllamaBalanceAmount(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Abs(value) <= 1e12
}

// ollamaLegacyBalanceWindow turns one remaining_percent entry into a window. A null
// entry is absent; a present but unusable entry is a parse failure.
func ollamaLegacyBalanceWindow(raw json.RawMessage, key string, durationSeconds int64) (Window, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ollamaTrimNull(raw), &fields); err != nil {
		return Window{}, false, err
	}
	remaining, ok := ollamaBalanceAmount(fields, "remaining_percent")
	if !ok || remaining < 0 || remaining > 100 {
		return Window{}, false, errors.New("unusable remaining_percent")
	}
	var reset *time.Time
	if resetRaw, hasReset := ollamaBalanceField(fields, "resets_at"); hasReset {
		t, ok := ollamaBalanceTimestamp(resetRaw)
		if !ok {
			return Window{}, false, errors.New("unusable resets_at")
		}
		reset = &t
	}
	window := normalizedWindow(key, 100-remaining, reset, durationSeconds)
	window.Pace = computeWindowPace(window, time.Now())
	return window, true, nil
}

// ollamaBalanceTimestamp validates an RFC 3339 timestamp with a real calendar date.
func ollamaBalanceTimestamp(raw json.RawMessage) (time.Time, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return time.Time{}, false
	}
	t, ok := parseClaudeTime(text)
	if !ok {
		return time.Time{}, false
	}
	return t, true
}

func responseFinalURL(resp *http.Response) *url.URL {
	if resp == nil || resp.Request == nil {
		return nil
	}
	return resp.Request.URL
}

func isUsageCrossOriginRedirect(resp *http.Response, expectedRawURL string) bool {
	finalURL := responseFinalURL(resp)
	if finalURL == nil {
		return false
	}
	expected, err := url.Parse(expectedRawURL)
	if err != nil {
		return false
	}
	return !strings.EqualFold(finalURL.Scheme, expected.Scheme) || !strings.EqualFold(finalURL.Host, expected.Host)
}

func isOllamaSignInRedirect(u *url.URL) bool {
	if u == nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	path := strings.ToLower(u.EscapedPath())
	switch {
	case (host == "ollama.com" || host == "www.ollama.com") && path == "/signin":
		return true
	case host == "signin.ollama.com":
		return true
	case strings.HasSuffix(host, ".workos.com") && strings.HasPrefix(path, "/user_management/authorize"):
		return true
	default:
		return false
	}
}

func parseOllamaSettingsHTML(data []byte) (Snapshot, *ErrorDetail, error) {
	html := string(data)
	detail := &ErrorDetail{Fields: map[string]string{}}
	plan := firstOllamaCapture(ollamaPlanAnchor, html)
	if plan == "" {
		plan = firstOllamaCapture(ollamaWalletPlanAnchor, html)
	}
	balance, hasBalance, sawWalletSpend := parseOllamaCreditWallet(html)

	windows := make([]Window, 0, 3)
	if w, ok := parseOllamaUsageBlock("session", []string{ollamaSessionAnchor, ollamaHourlyAnchor}, html, detail); ok {
		w.WindowDurationSeconds = ollamaSessionWindowSeconds
		w.Pace = computeWindowPace(w, time.Now())
		windows = append(windows, w)
	}
	if w, ok := parseOllamaUsageBlock("weekly", []string{ollamaWeeklyAnchor}, html, detail); ok {
		w.WindowDurationSeconds = ollamaWeeklyWindowSeconds
		w.Pace = computeWindowPace(w, time.Now())
		windows = append(windows, w)
	}
	if w, ok := parseOllamaUsageBlock("monthly", []string{ollamaMonthlyAnchor, ollamaFreeAnchor}, html, detail); ok {
		w.WindowDurationSeconds = ollamaMonthlyWindowSeconds
		w.Pace = computeWindowPace(w, time.Now())
		windows = append(windows, w)
	}
	windows = orderWindows(windows, []string{"session", "weekly", "monthly"})

	if len(windows) == 0 && !hasBalance && !sawWalletSpend {
		if ollamaSignedOutAnchor.MatchString(html) {
			return Snapshot{}, fieldDetail("page", "signed_out"), errors.New("Ollama Cloud settings page requires sign in.")
		}
		if strings.Contains(strings.ToLower(html), "cloud usage") {
			detail.Fields["windows"] = "missing"
			return Snapshot{Status: StatusOK, Plan: strings.TrimSpace(plan)}, detailOrNil(detail), nil
		}
		return Snapshot{}, fieldDetail("page", "markup_drift"), errors.New("Ollama Cloud settings page did not include recognized usage data.")
	}

	snap := Snapshot{Status: StatusOK, Plan: strings.TrimSpace(plan), Windows: windows}
	// A credit wallet is a balance, never a quota percentage: it shows as Credits even
	// beside reported meters, and a wallet without a balance still counts as usage data
	// so the reported spend is not dropped.
	if hasBalance {
		snap.Credits = &Credits{Label: "Credit balance", Balance: balance}
	}
	return snap, detailOrNil(detail), nil
}

// parseOllamaCreditWallet reads the current credit-wallet section: its balance and the
// monthly credits used. Both values are reported dollar amounts; no percentage is inferred.
func parseOllamaCreditWallet(html string) (balance float64, hasBalance, sawWalletSpend bool) {
	heading := ollamaWalletHeadingAnchor.FindStringIndex(html)
	if heading == nil {
		return 0, false, false
	}
	section := html[heading[1]:]
	if end := ollamaWalletSectionEnd.FindStringIndex(section); end != nil {
		section = section[:end[0]]
	}
	if raw := firstOllamaCapture(ollamaWalletBalanceAnchor, section); raw != "" {
		if value, ok := parseOllamaWalletAmount(raw); ok {
			balance, hasBalance = value, true
		}
	}
	if raw := firstOllamaCapture(ollamaWalletUsedAnchor, section); raw != "" {
		if _, ok := parseOllamaWalletAmount(raw); ok {
			sawWalletSpend = true
		}
	}
	return balance, hasBalance, sawWalletSpend
}

func parseOllamaWalletAmount(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(raw, "$"), ",", ""), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1e12 {
		return 0, false
	}
	return value, true
}

func parseOllamaUsageBlock(key string, labels []string, html string, detail *ErrorDetail) (Window, bool) {
	for _, label := range labels {
		match := ollamaLabelTextNode(label).FindStringIndex(html)
		if match == nil {
			continue
		}
		window := ollamaUsageBlock(html, label, match[0])
		percent, ok := parseOllamaPercent(window)
		if !ok {
			continue
		}
		var reset *time.Time
		if raw := firstOllamaCapture(ollamaResetAnchor, window); raw != "" {
			if t, ok := parseClaudeTime(raw); ok {
				reset = &t
			} else {
				detail.Fields[key+".reset_at"] = "string"
			}
		} else {
			detail.Fields[key+".reset_at"] = "missing"
		}
		return normalizedWindow(key, clampOllamaPercent(percent), reset, 0), true
	}
	return Window{}, false
}

func ollamaUsageBlock(html string, currentLabel string, start int) string {
	end := len(html)
	for _, label := range ollamaUsageAnchors {
		if label == currentLabel {
			continue
		}
		loc := ollamaLabelTextNode(label).FindStringIndex(html[start+1:])
		if loc == nil {
			continue
		}
		if boundary := start + 1 + loc[0]; boundary < end {
			end = boundary
		}
	}
	return html[start:end]
}

func parseOllamaPercent(text string) (float64, bool) {
	for _, pattern := range []*regexp.Regexp{ollamaPercentAnchor, ollamaWidthPercentAnchor} {
		raw := firstOllamaCapture(pattern, text)
		if raw == "" {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err == nil {
			return value, true
		}
	}
	return 0, false
}

func clampOllamaPercent(value float64) float64 {
	return clampUsagePercent(value)
}

func firstOllamaCapture(pattern *regexp.Regexp, text string) string {
	matches := pattern.FindStringSubmatch(text)
	if len(matches) < 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func ollamaStatus(status Status, message string, detail *ErrorDetail) Snapshot {
	return Snapshot{Status: status, Error: message, ErrorDetail: detail}
}

func NormalizeOllamaCookieHeader(input string) (string, error) {
	value, _, err := normalizeOllamaCookieHeader(input)
	return value, err
}
func NormalizeOllamaAuthMode(input string) string { return normalizeOllamaAuthMode(input) }
func NormalizeOllamaAPIKey(input string) string   { return normalizeOllamaAPIKey(input) }
