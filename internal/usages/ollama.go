package usages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

const (
	ollamaSettingsURL = "https://ollama.com/settings"
	ollamaTagsURL     = "https://ollama.com/api/tags"
	// This tracks a currently shipping Chrome-like user agent for the settings page.
	ollamaUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	ollamaAccept    = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

	OllamaAuthAuto   = "auto"
	OllamaAuthCookie = "cookie"
	OllamaAuthAPIKey = "api_key"

	ollamaSessionWindowSeconds int64 = 5 * 3600
	ollamaWeeklyWindowSeconds  int64 = 7 * 24 * 3600
)

var (
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaPlanAnchor = regexp.MustCompile(`(?is)Cloud Usage\s*</span\s*>\s*<span[^>]*>([^<]+)</span\s*>`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaEmailAnchor = regexp.MustCompile(`(?is)id=["']header-email["'][^>]*>([^<]+)<`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaSessionAnchor = "Session usage"
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaHourlyAnchor = "Hourly usage"
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaWeeklyAnchor = "Weekly usage"
	ollamaUsageAnchors = []string{ollamaSessionAnchor, ollamaHourlyAnchor, ollamaWeeklyAnchor}
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaPercentAnchor = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*%\s*used`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaWidthPercentAnchor = regexp.MustCompile(`(?i)width:\s*([0-9]+(?:\.[0-9]+)?)%`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaResetAnchor = regexp.MustCompile(`data-time=["']([^"']+)["']`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaSignedOutAnchor = regexp.MustCompile(`(?is)(sign in to ollama|log in to ollama|/auth/login|/auth/signin)`)

	ollamaHTTPClient = newUsageHTTPClient()
)

type ollamaCloudProvider struct{}

func NewOllamaCloudProvider() Provider { return ollamaCloudProvider{} }

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
		return ollamaStatus(StatusAuthMissing, "Ollama Cloud cookie or API key is not configured.", fieldDetail("config.ollama-cloud.auth", "missing")), nil
	}
}

func fetchOllamaWithCookie(ctx context.Context, cookie string) (Snapshot, error) {
	if cookie == "" {
		return ollamaStatus(StatusAuthMissing, "Ollama Cloud cookie is not configured.", fieldDetail("config.ollama-cloud.cookie", "missing")), nil
	}
	normalized, detail, err := normalizeOllamaCookieHeader(cookie)
	if err != nil {
		return ollamaStatus(StatusAuthMissing, "Ollama Cloud cookie is missing a recognized session cookie.", detail), nil
	}
	var signedOutErr error
	var signedOutDetail *ErrorDetail
	for _, candidate := range ollamaCookieCandidates(normalized) {
		body, detail, status, err := fetchOllamaSettingsHTML(ctx, candidate)
		if err != nil {
			snap := ollamaStatus(status, err.Error(), detail)
			if status == StatusFetchError {
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
		return ollamaStatus(StatusAuthMissing, signedOutErr.Error(), signedOutDetail), nil
	}
	return ollamaStatus(StatusFetchError, "Ollama Cloud settings page did not return usage data.", nil), nil
}

func fetchOllamaWithAPIKey(ctx context.Context, apiKey string) (Snapshot, error) {
	if apiKey == "" {
		return ollamaStatus(StatusAuthMissing, "Ollama Cloud API key is not configured.", fieldDetail("config.ollama-cloud.api_key", "missing")), nil
	}
	_, detail, status, err := fetchOllamaTags(ctx, apiKey)
	if err != nil {
		snap := ollamaStatus(status, err.Error(), detail)
		if status == StatusFetchError {
			return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
		}
		return snap, nil
	}
	return Snapshot{Provider: ProviderOllamaCloud, Status: StatusOK, Plan: "API key verified"}, nil
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
		regexp.MustCompile(`(?is)\bcookie:\s*'([^']+)'`),
		regexp.MustCompile(`(?is)\bcookie:\s*"([^"]+)"`),
		regexp.MustCompile(`(?im)\bcookie:\s*([^\r\n]+)`),
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
	case "session", "__Secure-session", "ollama_session", "__Host-ollama_session", "__Secure-next-auth.session-token", "next-auth.session-token":
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
	case resp.StatusCode == http.StatusOK:
		return data, nil, "", nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fieldDetail("settings", "auth"), StatusAuthMissing, errors.New("Ollama Cloud cookie was rejected.")
	default:
		return nil, fieldDetail("settings", fmt.Sprintf("http_%d", resp.StatusCode)), StatusFetchError, fmt.Errorf("Ollama Cloud settings endpoint returned HTTP %d.", resp.StatusCode)
	}
}

func fetchOllamaTags(ctx context.Context, apiKey string) ([]byte, *ErrorDetail, Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaTagsURL, nil)
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
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, jsonShapeDetail("tags", data), StatusAuthMissing, errors.New("Ollama Cloud API key was rejected.")
	default:
		return nil, jsonShapeDetail("tags", data), StatusFetchError, fmt.Errorf("Ollama Cloud API endpoint returned HTTP %d.", resp.StatusCode)
	}
	var raw struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, jsonShapeDetail("tags", data), StatusFetchError, errors.New("Ollama Cloud API response could not be decoded.")
	}
	return data, nil, "", nil
}

func parseOllamaSettingsHTML(data []byte) (Snapshot, *ErrorDetail, error) {
	html := string(data)
	detail := &ErrorDetail{Fields: map[string]string{}}
	plan := firstOllamaCapture(ollamaPlanAnchor, html)

	windows := make([]Window, 0, 2)
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
	windows = orderWindows(windows, []string{"session", "weekly"})

	if len(windows) == 0 {
		if ollamaSignedOutAnchor.MatchString(html) {
			return Snapshot{}, fieldDetail("page", "signed_out"), errors.New("Ollama Cloud settings page requires sign in.")
		}
		if strings.Contains(strings.ToLower(html), "cloud usage") {
			detail.Fields["windows"] = "missing"
			return Snapshot{Provider: ProviderOllamaCloud, Status: StatusOK, Plan: strings.TrimSpace(plan)}, detailOrNil(detail), nil
		}
		return Snapshot{}, fieldDetail("page", "markup_drift"), errors.New("Ollama Cloud settings page did not include recognized usage data.")
	}

	return Snapshot{Provider: ProviderOllamaCloud, Status: StatusOK, Plan: strings.TrimSpace(plan), Windows: windows}, detailOrNil(detail), nil
}

func parseOllamaUsageBlock(key string, labels []string, html string, detail *ErrorDetail) (Window, bool) {
	for _, label := range labels {
		index := strings.Index(html, label)
		if index < 0 {
			continue
		}
		window := ollamaUsageBlock(html, label, index)
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
		return Window{Key: key, PercentUsed: clampOllamaPercent(percent), ResetAt: reset}, true
	}
	return Window{}, false
}

func ollamaUsageBlock(html string, currentLabel string, start int) string {
	end := len(html)
	for _, label := range ollamaUsageAnchors {
		if label == currentLabel {
			continue
		}
		next := strings.Index(html[start+1:], label)
		if next < 0 {
			continue
		}
		next += start + 1
		if next < end {
			end = next
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
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func firstOllamaCapture(pattern *regexp.Regexp, text string) string {
	matches := pattern.FindStringSubmatch(text)
	if len(matches) < 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func ollamaStatus(status Status, message string, detail *ErrorDetail) Snapshot {
	return Snapshot{Provider: ProviderOllamaCloud, Status: status, Error: message, ErrorDetail: detail}
}
