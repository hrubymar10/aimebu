package usages

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

const (
	copilotDeviceCodeURL    = "https://github.com/login/device/code"
	copilotAccessTokenURL   = "https://github.com/login/oauth/access_token"
	copilotClientID         = "Iv1.b507a08c87ecfe98"
	copilotScope            = "read:user"
	copilotGrantType        = "urn:ietf:params:oauth:grant-type:device_code"
	copilotDefaultAPIHost   = "https://api.github.com"
	copilotUsagePath        = "/copilot_internal/user"
	copilotEditorVersion    = "vscode/1.96.2"
	copilotPluginVersion    = "copilot-chat/0.26.7"
	copilotUserAgent        = "GitHubCopilotChat/0.26.7"
	copilotGitHubAPIVersion = "2025-04-01"
	copilotMaxPollInterval  = 60
)

type copilotProvider struct{}

func NewCopilotProvider() Provider { return copilotProvider{} }

func (copilotProvider) Key() string { return ProviderGitHubCopilot }

func (copilotProvider) Fetch(ctx context.Context, store *Store) (Snapshot, error) {
	cfg, err := store.LoadConfig()
	if err != nil {
		return copilotStatus(StatusFetchError, "Copilot usage config could not be read.", nil), nil
	}
	pc := cfg.Providers[ProviderGitHubCopilot]
	token := strings.TrimSpace(pc.Token)
	if token == "" {
		return copilotStatus(StatusAuthMissing, "GitHub Copilot is not signed in.", fieldDetail("config.github-copilot.token", "missing")), nil
	}
	apiBase, err := normalizeCopilotEnterpriseHost(pc.EnterpriseHost)
	if err != nil {
		return copilotStatus(StatusFetchError, err.Error(), fieldDetail("config.github-copilot.enterprise_host", "invalid")), nil
	}
	raw, detail, status, err := fetchCopilotUsage(ctx, token, apiBase)
	if err != nil {
		snap := copilotStatus(status, err.Error(), detail)
		if status == StatusFetchError {
			return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
		}
		return snap, nil
	}
	snap, detail, err := normalizeCopilotUsage(raw)
	if err != nil {
		snap = copilotStatus(StatusFetchError, err.Error(), detail)
		return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
	}
	if detail != nil && len(detail.Fields) > 0 {
		snap.ErrorDetail = detail
	}
	return snap, nil
}

func normalizeCopilotEnterpriseHost(input string) (string, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return copilotDefaultAPIHost, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("Copilot enterprise host must be an https URL.")
	}
	if u.Scheme != "https" {
		return "", errors.New("Copilot enterprise host must use https.")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("Copilot enterprise host must not include a path.")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("Copilot enterprise host must not include query, fragment, or credentials.")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Host, "."))
	if host == "" {
		return "", errors.New("Copilot enterprise host must include a host.")
	}
	if !strings.HasPrefix(host, "api.") {
		host = "api." + host
	}
	return "https://" + host, nil
}

func copilotUsageURL(apiBase string) (string, error) {
	base, err := url.Parse(apiBase)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return "", errors.New("Copilot API host is invalid.")
	}
	base.Path = copilotUsagePath
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

type copilotUsageRaw struct {
	QuotaSnapshots    *copilotQuotaSnapshotsRaw `json:"quota_snapshots"`
	CopilotPlan       string                    `json:"copilot_plan"`
	AssignedDate      string                    `json:"assigned_date"`
	QuotaResetDate    string                    `json:"quota_reset_date"`
	MonthlyQuotas     *copilotQuotaCountsRaw    `json:"monthly_quotas"`
	LimitedUserQuotas *copilotQuotaCountsRaw    `json:"limited_user_quotas"`
}

type copilotQuotaSnapshotsRaw struct {
	PremiumInteractions *copilotQuotaSnapshotRaw `json:"premium_interactions"`
	Chat                *copilotQuotaSnapshotRaw `json:"chat"`
}

type copilotQuotaCountsRaw struct {
	Chat        *float64 `json:"chat"`
	Completions *float64 `json:"completions"`
}

type copilotQuotaSnapshotRaw struct {
	Entitlement         *float64 `json:"entitlement"`
	Remaining           *float64 `json:"remaining"`
	PercentRemaining    *float64 `json:"percent_remaining"`
	QuotaID             string   `json:"quota_id"`
	HasPercentRemaining bool     `json:"-"`
}

func (q *copilotQuotaSnapshotRaw) UnmarshalJSON(data []byte) error {
	var raw struct {
		Entitlement      json.RawMessage `json:"entitlement"`
		Remaining        json.RawMessage `json:"remaining"`
		PercentRemaining json.RawMessage `json:"percent_remaining"`
		QuotaID          string          `json:"quota_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	q.Entitlement = decodeCopilotNumber(raw.Entitlement)
	q.Remaining = decodeCopilotNumber(raw.Remaining)
	q.PercentRemaining = decodeCopilotNumber(raw.PercentRemaining)
	q.HasPercentRemaining = q.PercentRemaining != nil
	if q.PercentRemaining == nil && q.Entitlement != nil && *q.Entitlement > 0 && q.Remaining != nil {
		percent := (*q.Remaining / *q.Entitlement) * 100
		q.PercentRemaining = &percent
		q.HasPercentRemaining = true
	}
	q.QuotaID = raw.QuotaID
	return nil
}

func (q *copilotQuotaCountsRaw) UnmarshalJSON(data []byte) error {
	var raw struct {
		Chat        json.RawMessage `json:"chat"`
		Completions json.RawMessage `json:"completions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	q.Chat = decodeCopilotNumber(raw.Chat)
	q.Completions = decodeCopilotNumber(raw.Completions)
	return nil
}

func decodeCopilotNumber(data json.RawMessage) *float64 {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var n float64
	if err := json.Unmarshal(data, &n); err == nil {
		return &n
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if parsed, err := parseFloat(s); err == nil {
			return &parsed
		}
	}
	return nil
}

func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}

func fetchCopilotUsage(ctx context.Context, token, apiBase string) (copilotUsageRaw, *ErrorDetail, Status, error) {
	usageURL, err := copilotUsageURL(apiBase)
	if err != nil {
		return copilotUsageRaw{}, nil, StatusFetchError, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return copilotUsageRaw{}, nil, StatusFetchError, err
	}
	req.Header.Set("Authorization", "token "+token)
	addCopilotHeaders(req.Header)
	resp, err := doWithRetry(ctx, usageHTTPClient, req, RetryPolicy{MaxRetries: 1})
	if err != nil {
		return copilotUsageRaw{}, nil, usageRequestStatus(err), fmt.Errorf("GitHub Copilot usage request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return copilotUsageRaw{}, jsonShapeDetail("usage", data), StatusAuthMissing, errors.New("GitHub Copilot token was rejected.")
	case http.StatusForbidden:
		return copilotUsageRaw{}, jsonShapeDetail("usage", data), StatusScopeMissing, errors.New("GitHub Copilot token lacks access to usage data.")
	default:
		return copilotUsageRaw{}, jsonShapeDetail("usage", data), StatusFetchError, fmt.Errorf("GitHub Copilot usage endpoint returned HTTP %d.", resp.StatusCode)
	}
	var raw copilotUsageRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return copilotUsageRaw{}, jsonShapeDetail("usage", data), StatusFetchError, errors.New("GitHub Copilot usage response could not be decoded.")
	}
	return raw, nil, "", nil
}

func addCopilotHeaders(h http.Header) {
	h.Set("Accept", "application/json")
	h.Set("Editor-Version", copilotEditorVersion)
	h.Set("Editor-Plugin-Version", copilotPluginVersion)
	h.Set("User-Agent", copilotUserAgent)
	h.Set("X-Github-Api-Version", copilotGitHubAPIVersion)
}

func normalizeCopilotUsage(raw copilotUsageRaw) (Snapshot, *ErrorDetail, error) {
	detail := &ErrorDetail{Fields: map[string]string{}}
	reset, ok := parseCopilotTime(raw.QuotaResetDate)
	if strings.TrimSpace(raw.QuotaResetDate) != "" && !ok {
		detail.Fields["quota_reset_date"] = "string"
	}
	premium, chat := copilotQuotaWindows(raw)
	windows := make([]Window, 0, 2)
	add := func(key string, q *copilotQuotaSnapshotRaw) {
		if q == nil || q.PercentRemaining == nil || !q.HasPercentRemaining {
			return
		}
		used := 100 - *q.PercentRemaining
		windows = append(windows, Window{Key: key, PercentUsed: used, ResetAt: reset})
	}
	add("premium", premium)
	add("chat", chat)
	if len(windows) == 0 {
		data, err := json.Marshal(raw)
		if err != nil {
			return Snapshot{}, fieldDetail("usage", "marshal_error"), errors.New("GitHub Copilot usage response could not be inspected.")
		}
		return Snapshot{}, jsonShapeDetail("usage", data), errors.New("GitHub Copilot usage response did not include recognized quota windows.")
	}
	return Snapshot{
		Provider: ProviderGitHubCopilot,
		Status:   StatusOK,
		Plan:     strings.TrimSpace(raw.CopilotPlan),
		Windows:  windows,
	}, detailOrNil(detail), nil
}

func copilotQuotaWindows(raw copilotUsageRaw) (*copilotQuotaSnapshotRaw, *copilotQuotaSnapshotRaw) {
	var premium, chat *copilotQuotaSnapshotRaw
	if raw.QuotaSnapshots != nil {
		premium = usableCopilotQuota(raw.QuotaSnapshots.PremiumInteractions)
		chat = usableCopilotQuota(raw.QuotaSnapshots.Chat)
	}
	fallback := makeCopilotQuotaSnapshots(raw.MonthlyQuotas, raw.LimitedUserQuotas)
	if premium == nil && fallback != nil {
		premium = usableCopilotQuota(fallback.PremiumInteractions)
	}
	if chat == nil && fallback != nil {
		chat = usableCopilotQuota(fallback.Chat)
	}
	return premium, chat
}

func usableCopilotQuota(q *copilotQuotaSnapshotRaw) *copilotQuotaSnapshotRaw {
	if q == nil || !q.HasPercentRemaining || q.PercentRemaining == nil {
		return nil
	}
	if q.Entitlement != nil && q.Remaining != nil && *q.Entitlement == 0 && *q.Remaining == 0 && *q.PercentRemaining == 0 && q.QuotaID == "" {
		return nil
	}
	return q
}

func makeCopilotQuotaSnapshots(monthly, limited *copilotQuotaCountsRaw) *copilotQuotaSnapshotsRaw {
	if monthly == nil || limited == nil {
		return nil
	}
	return &copilotQuotaSnapshotsRaw{
		PremiumInteractions: makeCopilotQuotaSnapshot(monthly.Completions, limited.Completions, "completions"),
		Chat:                makeCopilotQuotaSnapshot(monthly.Chat, limited.Chat, "chat"),
	}
}

func makeCopilotQuotaSnapshot(monthly, limited *float64, quotaID string) *copilotQuotaSnapshotRaw {
	if monthly == nil || limited == nil || *monthly <= 0 {
		return nil
	}
	entitlement := maxFloat(0, *monthly)
	remaining := maxFloat(0, *limited)
	percent := maxFloat(0, minFloat(100, (remaining/entitlement)*100))
	return &copilotQuotaSnapshotRaw{
		Entitlement:         &entitlement,
		Remaining:           &remaining,
		PercentRemaining:    &percent,
		QuotaID:             quotaID,
		HasPercentRemaining: true,
	}
}

func parseCopilotTime(value string) (*time.Time, bool) {
	if strings.TrimSpace(value) == "" {
		return nil, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			utc := t.UTC()
			return &utc, true
		}
	}
	return nil, false
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

type copilotFlowStore struct {
	mu    sync.Mutex
	flows map[string]*copilotFlowState
	now   func() time.Time
}

type copilotFlowState struct {
	FlowID                  string
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                int
	ExpiresAt               time.Time
	LastPollAt              time.Time
	LastResult              copilotPollResponse
	Terminal                bool
}

type copilotDeviceStartResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type copilotAccessTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
}

type copilotStartResponse struct {
	FlowID                  string    `json:"flow_id"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	Interval                int       `json:"interval"`
	ExpiresAt               time.Time `json:"expires_at"`
}

type copilotPollResponse struct {
	Status     string `json:"status"`
	Interval   int    `json:"interval,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
	Error      string `json:"error,omitempty"`
}

func newCopilotFlowStore() *copilotFlowStore {
	return &copilotFlowStore{flows: map[string]*copilotFlowState{}, now: time.Now}
}

func (s *copilotFlowStore) start(ctx context.Context, enterpriseHost string) (copilotStartResponse, string, error) {
	if _, err := normalizeCopilotEnterpriseHost(enterpriseHost); err != nil {
		return copilotStartResponse{}, "", err
	}
	raw, err := requestCopilotDeviceCode(ctx)
	if err != nil {
		return copilotStartResponse{}, "", err
	}
	if raw.Interval <= 0 {
		raw.Interval = 5
	}
	flowID, err := randomFlowID()
	if err != nil {
		return copilotStartResponse{}, "", err
	}
	expiresAt := s.now().Add(time.Duration(raw.ExpiresIn) * time.Second).UTC()
	state := &copilotFlowState{
		FlowID:                  flowID,
		DeviceCode:              raw.DeviceCode,
		UserCode:                raw.UserCode,
		VerificationURI:         raw.VerificationURI,
		VerificationURIComplete: raw.VerificationURIComplete,
		Interval:                raw.Interval,
		ExpiresAt:               expiresAt,
		LastResult:              copilotPollResponse{Status: "pending", Interval: raw.Interval},
	}
	s.mu.Lock()
	s.flows[flowID] = state
	s.mu.Unlock()
	return copilotStartResponse{
		FlowID:                  flowID,
		UserCode:                raw.UserCode,
		VerificationURI:         raw.VerificationURI,
		VerificationURIComplete: raw.VerificationURIComplete,
		Interval:                raw.Interval,
		ExpiresAt:               expiresAt,
	}, raw.DeviceCode, nil
}

func (s *copilotFlowStore) poll(ctx context.Context, flowID string) (copilotPollResponse, string, error) {
	s.mu.Lock()
	state := s.flows[flowID]
	if state == nil {
		s.mu.Unlock()
		return copilotPollResponse{Status: "expired"}, "", nil
	}
	now := s.now()
	if now.After(state.ExpiresAt) {
		delete(s.flows, flowID)
		s.mu.Unlock()
		return copilotPollResponse{Status: "expired"}, "", nil
	}
	if !state.LastPollAt.IsZero() && now.Sub(state.LastPollAt) < time.Duration(state.Interval)*time.Second {
		resp := state.LastResult
		resp.RetryAfter = int((time.Duration(state.Interval)*time.Second - now.Sub(state.LastPollAt) + time.Second - 1) / time.Second)
		s.mu.Unlock()
		return resp, "", nil
	}
	state.LastPollAt = now
	if state.LastResult.Status == "" {
		state.LastResult = copilotPollResponse{Status: "pending", Interval: state.Interval}
	}
	deviceCode := state.DeviceCode
	interval := state.Interval
	s.mu.Unlock()

	token, status, err := requestCopilotAccessToken(ctx, deviceCode)
	s.mu.Lock()
	defer s.mu.Unlock()
	state = s.flows[flowID]
	if err != nil {
		resp := copilotPollResponse{Status: "network_error", Interval: interval, Error: err.Error()}
		if state != nil {
			state.LastResult = resp
		}
		return resp, "", nil
	}
	switch status {
	case "":
		if state != nil {
			delete(s.flows, flowID)
		}
		return copilotPollResponse{Status: "success"}, token, nil
	case "authorization_pending":
		if state != nil {
			interval = state.Interval
		}
		resp := copilotPollResponse{Status: "pending", Interval: interval}
		if state != nil {
			state.LastResult = resp
		}
		return resp, "", nil
	case "slow_down":
		nextInterval := minInt(copilotMaxPollInterval, maxInt(1, interval*2))
		if state != nil {
			state.Interval = minInt(copilotMaxPollInterval, maxInt(1, state.Interval*2))
			nextInterval = state.Interval
		}
		resp := copilotPollResponse{Status: "slow_down", Interval: nextInterval, RetryAfter: nextInterval}
		if state != nil {
			state.LastResult = resp
		}
		return resp, "", nil
	case "expired_token":
		if state != nil {
			delete(s.flows, flowID)
		}
		return copilotPollResponse{Status: "expired"}, "", nil
	case "access_denied":
		if state != nil {
			delete(s.flows, flowID)
		}
		return copilotPollResponse{Status: "denied"}, "", nil
	default:
		if state != nil {
			delete(s.flows, flowID)
		}
		return copilotPollResponse{Status: "denied"}, "", nil
	}
}

func requestCopilotDeviceCode(ctx context.Context) (copilotDeviceStartResponse, error) {
	form := url.Values{}
	form.Set("client_id", copilotClientID)
	form.Set("scope", copilotScope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, copilotDeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return copilotDeviceStartResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := usageHTTPClient.Do(req)
	if err != nil {
		return copilotDeviceStartResponse{}, errors.New("GitHub device-code request failed.")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return copilotDeviceStartResponse{}, fmt.Errorf("GitHub device-code endpoint returned HTTP %d.", resp.StatusCode)
	}
	var raw copilotDeviceStartResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return copilotDeviceStartResponse{}, errors.New("GitHub device-code response could not be decoded.")
	}
	if raw.DeviceCode == "" || raw.UserCode == "" || raw.VerificationURI == "" {
		return copilotDeviceStartResponse{}, errors.New("GitHub device-code response was missing required fields.")
	}
	return raw, nil
}

func requestCopilotAccessToken(ctx context.Context, deviceCode string) (string, string, error) {
	form := url.Values{}
	form.Set("client_id", copilotClientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", copilotGrantType)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, copilotAccessTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := usageHTTPClient.Do(req)
	if err != nil {
		return "", "", errors.New("GitHub device-token request failed.")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var raw copilotAccessTokenResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", errors.New("GitHub device-token response could not be decoded.")
	}
	if raw.Error != "" {
		return "", raw.Error, nil
	}
	if raw.AccessToken == "" {
		return "", "", errors.New("GitHub device-token response did not include an access token.")
	}
	return raw.AccessToken, "", nil
}

func randomFlowID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func copilotStatus(status Status, message string, detail *ErrorDetail) Snapshot {
	return Snapshot{Provider: ProviderGitHubCopilot, Status: status, Error: message, ErrorDetail: detail}
}
