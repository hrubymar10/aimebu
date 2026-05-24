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
)

const (
	ollamaSettingsURL = "https://ollama.com/settings"
	// This tracks a currently shipping Chrome-like user agent for the settings page.
	ollamaUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	ollamaAccept    = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
)

var (
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaPlanAnchor = regexp.MustCompile(`(?is)Cloud Usage\s*</span>\s*<span[^>]*>([^<]+)</span>`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaEmailAnchor = regexp.MustCompile(`(?is)id=["']header-email["'][^>]*>([^<]+)<`)
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaSessionAnchor = "Session usage"
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaHourlyAnchor = "Hourly usage"
	// if this anchor drifts, the page markup has changed since this parser was written; capture the new page and re-pin
	ollamaWeeklyAnchor = "Weekly usage"
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
	cookie := strings.TrimSpace(cfg.Providers[ProviderOllamaCloud].Cookie)
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
	resp, err := ollamaHTTPClient.Do(req)
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

func parseOllamaSettingsHTML(data []byte) (Snapshot, *ErrorDetail, error) {
	html := string(data)
	detail := &ErrorDetail{Fields: map[string]string{}}
	plan := firstOllamaCapture(ollamaPlanAnchor, html)

	windows := make([]Window, 0, 2)
	if w, ok := parseOllamaUsageBlock("session", []string{ollamaSessionAnchor, ollamaHourlyAnchor}, html, detail); ok {
		windows = append(windows, w)
	}
	if w, ok := parseOllamaUsageBlock("weekly", []string{ollamaWeeklyAnchor}, html, detail); ok {
		windows = append(windows, w)
	}
	windows = orderWindows(windows, []string{"session", "weekly"})

	if len(windows) == 0 {
		if ollamaSignedOutAnchor.MatchString(html) {
			return Snapshot{}, fieldDetail("page", "signed_out"), errors.New("Ollama Cloud settings page requires sign in.")
		}
		if strings.Contains(html, "Cloud Usage") {
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
		end := index + len(label) + 800
		if end > len(html) {
			end = len(html)
		}
		window := html[index:end]
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
