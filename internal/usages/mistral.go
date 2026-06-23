package usages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

const (
	mistralConsoleVibeUsageURL = "https://console.mistral.ai/api-ui/trpc/billing.vibeUsage?batch=1&input=%7B%220%22%3A%7B%22json%22%3Anull%2C%22meta%22%3A%7B%22values%22%3A%5B%22undefined%22%5D%2C%22v%22%3A1%7D%7D%7D"
	mistralAdminUsageURL       = "https://admin.mistral.ai/api/billing/v2/usage"
	mistralAdminOrigin         = "https://admin.mistral.ai"
	mistralAdminReferer        = "https://admin.mistral.ai/organization/usage"
)

var mistralHTTPClient = newUsageHTTPClient()

type mistralProvider struct{}

func NewMistralProvider() Provider { return mistralProvider{} }

func (mistralProvider) Key() string { return ProviderMistral }

func (mistralProvider) Fetch(ctx context.Context, store *Store) (Snapshot, error) {
	cfg, err := store.LoadConfig()
	if err != nil {
		return mistralStatus(StatusFetchError, "Mistral usage config could not be read.", nil), nil
	}
	cookie := strings.TrimSpace(cfg.Providers[ProviderMistral].Cookie)
	if cookie == "" {
		return mistralStatus(StatusAuthMissing, "Mistral cookie is not configured.", fieldDetail("config.mistral.cookie", "missing")), nil
	}
	parsed, detail, err := parseMistralCookieHeader(cookie)
	if err != nil {
		return mistralStatus(StatusAuthMissing, err.Error(), detail), nil
	}

	vibe, detail, status, err := fetchMistralVibeUsage(ctx, parsed)
	if err != nil {
		snap := mistralStatus(status, err.Error(), detail)
		if status == StatusFetchError || status == StatusTimeout {
			return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
		}
		return snap, nil
	}
	snap, detail, err := normalizeMistralVibeUsage(vibe)
	if err != nil {
		snap := mistralStatus(StatusFetchError, err.Error(), detail)
		return Snapshot{}, &SnapshotError{Snapshot: snap, Err: err}
	}

	if vibe.PaygEnabled {
		spend, spendDetail, spendStatus, spendErr := fetchMistralAPISpend(ctx, parsed)
		if spendErr == nil {
			if credits, plan, ok := normalizeMistralSpend(spend); ok {
				snap.Credits = credits
				if plan != "" {
					snap.Plan = plan
				}
			}
		} else if spendStatus == StatusAuthMissing || spendStatus == StatusFetchError || spendStatus == StatusTimeout {
			// The Vibe quota is the primary value; an unavailable API-spend
			// fallback should not turn a working monthly window into an error.
			detail = mergeErrorDetails(detail, spendDetail)
		}
	}

	snap.ErrorDetail = detailOrNil(detail)
	return snap, nil
}

type mistralCookies struct {
	pairs map[string]string
}

func normalizeMistralCookieHeader(input string) (string, *ErrorDetail, error) {
	parsed, detail, err := parseMistralCookieHeader(input)
	if err != nil {
		return "", detail, err
	}
	return parsed.Serialize(), nil, nil
}

func parseMistralCookieHeader(input string) (mistralCookies, *ErrorDetail, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return mistralCookies{}, fieldDetail("cookie", "missing"), errors.New("Mistral cookie is empty.")
	}
	if extracted := extractOllamaCookieHeader(value); extracted != "" {
		value = extracted
	}
	value = strings.TrimSpace(stripWrappingCookieQuotes(value))
	if strings.HasPrefix(strings.ToLower(value), "cookie:") {
		value = strings.TrimSpace(value[len("cookie:"):])
	}
	if strings.ContainsAny(value, "\r\n") {
		return mistralCookies{}, fieldDetail("cookie", "invalid"), errors.New("Mistral cookie contains invalid line breaks.")
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
		return mistralCookies{}, fieldDetail("cookie", "missing"), errors.New("Mistral cookie did not contain cookie pairs.")
	}
	parsed := mistralCookies{pairs: pairs}
	if parsed.CSRFToken() == "" {
		return mistralCookies{}, fieldDetail("cookie.csrftoken", "missing"), errors.New("Mistral cookie is missing csrftoken.")
	}
	if err := validateMistralCSRFToken(parsed.CSRFToken()); err != nil {
		return mistralCookies{}, fieldDetail("cookie.csrftoken", "invalid"), errors.New("Mistral csrftoken is invalid.")
	}
	if !parsed.HasOrySession() {
		return mistralCookies{}, fieldDetail("cookie.ory_session", "missing"), errors.New("Mistral cookie is missing an ory_session_* cookie; paste the full Cookie header from console.mistral.ai.")
	}
	return parsed, nil, nil
}

func (c mistralCookies) CSRFToken() string {
	return strings.TrimSpace(c.pairs["csrftoken"])
}

func (c mistralCookies) HasOrySession() bool {
	for name := range c.pairs {
		if isMistralOrySessionCookie(name) {
			return true
		}
	}
	return false
}

func (c mistralCookies) VibeCookieHeader() string {
	out := map[string]string{"csrftoken": c.CSRFToken()}
	for name, value := range c.pairs {
		if isMistralOrySessionCookie(name) {
			out[name] = value
		}
	}
	return serializeMistralCookies(out)
}

func (c mistralCookies) Serialize() string {
	return serializeMistralCookies(c.pairs)
}

func isMistralOrySessionCookie(name string) bool {
	return strings.HasPrefix(name, "ory_session_")
}

func validateMistralCSRFToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, ";,\r\n") {
		return errors.New("invalid csrftoken")
	}
	return nil
}

func serializeMistralCookies(pairs map[string]string) string {
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

type mistralVibeUsageRaw struct {
	UsagePercentage       float64
	QuotaChangedThisMonth bool
	PaygEnabled           bool
	ResetAt               string
}

func fetchMistralVibeUsage(ctx context.Context, cookies mistralCookies) (mistralVibeUsageRaw, *ErrorDetail, Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mistralConsoleVibeUsageURL, nil)
	if err != nil {
		return mistralVibeUsageRaw{}, nil, StatusFetchError, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cookie", cookies.VibeCookieHeader())
	req.Header.Set("X-CSRFToken", cookies.CSRFToken())
	resp, err := doWithRetry(ctx, mistralHTTPClient, req, RetryPolicy{MaxRetries: 1})
	if err != nil {
		return mistralVibeUsageRaw{}, nil, usageRequestStatus(err), fmt.Errorf("Mistral Vibe usage request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return mistralVibeUsageRaw{}, jsonShapeDetail("vibe_usage", data), StatusAuthMissing, errors.New("Mistral cookie was rejected.")
	default:
		return mistralVibeUsageRaw{}, jsonShapeDetail("vibe_usage", data), StatusFetchError, fmt.Errorf("Mistral Vibe usage endpoint returned HTTP %d.", resp.StatusCode)
	}
	raw, detail, err := decodeMistralVibeUsage(data)
	if err != nil {
		return mistralVibeUsageRaw{}, detail, StatusFetchError, err
	}
	return raw, nil, "", nil
}

func decodeMistralVibeUsage(data []byte) (mistralVibeUsageRaw, *ErrorDetail, error) {
	var responses []struct {
		Result struct {
			Data struct {
				JSON struct {
					UsagePercentage       *float64 `json:"usage_percentage"`
					QuotaChangedThisMonth bool     `json:"quota_changed_this_month"`
					PaygEnabled           bool     `json:"payg_enabled"`
					ResetAt               string   `json:"reset_at"`
				} `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &responses); err != nil {
		return mistralVibeUsageRaw{}, jsonShapeDetail("vibe_usage", data), errors.New("Mistral Vibe usage response could not be decoded.")
	}
	if len(responses) == 0 || responses[0].Result.Data.JSON.UsagePercentage == nil {
		return mistralVibeUsageRaw{}, jsonShapeDetail("vibe_usage", data), errors.New("Mistral Vibe usage response did not include usage_percentage.")
	}
	jsonValue := responses[0].Result.Data.JSON
	return mistralVibeUsageRaw{
		UsagePercentage:       *jsonValue.UsagePercentage,
		QuotaChangedThisMonth: jsonValue.QuotaChangedThisMonth,
		PaygEnabled:           jsonValue.PaygEnabled,
		ResetAt:               jsonValue.ResetAt,
	}, nil, nil
}

func normalizeMistralVibeUsage(raw mistralVibeUsageRaw) (Snapshot, *ErrorDetail, error) {
	if math.IsNaN(raw.UsagePercentage) || math.IsInf(raw.UsagePercentage, 0) || raw.UsagePercentage < 0 || raw.UsagePercentage > 100 {
		return Snapshot{}, fieldDetail("vibe_usage.0.result.data.json.usage_percentage", "number_out_of_range"), errors.New("Mistral Vibe usage percentage was outside 0..100.")
	}
	detail := &ErrorDetail{Fields: map[string]string{}}
	var reset *time.Time
	if raw.ResetAt != "" {
		if t, ok := parseClaudeTime(raw.ResetAt); ok {
			reset = &t
		} else {
			detail.Fields["vibe_usage.reset_at"] = "string"
		}
	} else {
		detail.Fields["vibe_usage.reset_at"] = "missing"
	}
	window := Window{Key: "monthly", PercentUsed: raw.UsagePercentage, ResetAt: reset}
	if reset != nil {
		start := time.Date(reset.UTC().Year(), reset.UTC().Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
		window.WindowDurationSeconds = int64(reset.Sub(start).Seconds())
		window.Pace = computeWindowPace(window, time.Now())
	}
	return Snapshot{Provider: ProviderMistral, Status: StatusOK, Plan: "Vibe monthly quota", Windows: []Window{window}}, detailOrNil(detail), nil
}

type mistralBillingResponse struct {
	Completion   *mistralModelUsageCategory     `json:"completion"`
	OCR          *mistralModelUsageCategory     `json:"ocr"`
	Connectors   *mistralModelUsageCategory     `json:"connectors"`
	Audio        *mistralModelUsageCategory     `json:"audio"`
	LibrariesAPI *mistralLibrariesUsageCategory `json:"libraries_api"`
	FineTuning   *mistralFineTuningCategory     `json:"fine_tuning"`
	Currency     string                         `json:"currency"`
	Prices       []mistralPrice                 `json:"prices"`
}

type mistralModelUsageCategory struct {
	Models map[string]mistralModelUsageData `json:"models"`
}

type mistralLibrariesUsageCategory struct {
	Pages  *mistralModelUsageCategory `json:"pages"`
	Tokens *mistralModelUsageCategory `json:"tokens"`
}

type mistralFineTuningCategory struct {
	Training map[string]mistralModelUsageData `json:"training"`
	Storage  map[string]mistralModelUsageData `json:"storage"`
}

type mistralModelUsageData struct {
	Input  []mistralUsageEntry `json:"input"`
	Output []mistralUsageEntry `json:"output"`
	Cached []mistralUsageEntry `json:"cached"`
}

type mistralUsageEntry struct {
	BillingMetric string `json:"billing_metric"`
	BillingGroup  string `json:"billing_group"`
	Value         int    `json:"value"`
	ValuePaid     *int   `json:"value_paid"`
}

type mistralPrice struct {
	BillingMetric string `json:"billing_metric"`
	BillingGroup  string `json:"billing_group"`
	Price         string `json:"price"`
}

func fetchMistralAPISpend(ctx context.Context, cookies mistralCookies) (mistralBillingResponse, *ErrorDetail, Status, error) {
	now := time.Now().UTC()
	u, err := url.Parse(mistralAdminUsageURL)
	if err != nil {
		return mistralBillingResponse{}, nil, StatusFetchError, err
	}
	q := u.Query()
	q.Set("month", strconv.Itoa(int(now.Month())))
	q.Set("year", strconv.Itoa(now.Year()))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return mistralBillingResponse{}, nil, StatusFetchError, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cookie", cookies.Serialize())
	req.Header.Set("X-CSRFTOKEN", cookies.CSRFToken())
	req.Header.Set("Origin", mistralAdminOrigin)
	req.Header.Set("Referer", mistralAdminReferer)
	resp, err := doWithRetry(ctx, mistralHTTPClient, req, RetryPolicy{MaxRetries: 1})
	if err != nil {
		return mistralBillingResponse{}, nil, usageRequestStatus(err), fmt.Errorf("Mistral API spend request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return mistralBillingResponse{}, jsonShapeDetail("usage", data), StatusAuthMissing, errors.New("Mistral API spend cookie was rejected.")
	default:
		return mistralBillingResponse{}, jsonShapeDetail("usage", data), StatusFetchError, fmt.Errorf("Mistral API spend endpoint returned HTTP %d.", resp.StatusCode)
	}
	var raw mistralBillingResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return mistralBillingResponse{}, jsonShapeDetail("usage", data), StatusFetchError, errors.New("Mistral API spend response could not be decoded.")
	}
	return raw, nil, "", nil
}

func normalizeMistralSpend(raw mistralBillingResponse) (*Credits, string, bool) {
	prices := mistralPriceIndex(raw.Prices)
	var input, output, cached, modelCount int
	var spend float64
	for _, models := range mistralSpendModelGroups(raw) {
		for _, model := range models {
			modelCount++
			in, out, ca, cost := aggregateMistralModel(model, prices)
			input += in
			output += out
			cached += ca
			spend += cost
		}
	}
	if spend <= 0 {
		return nil, mistralPlanText(modelCount, input, output, cached), false
	}
	currency := strings.TrimSpace(raw.Currency)
	if currency == "" {
		currency = "EUR"
	}
	return &Credits{Label: "Monthly spend (" + currency + ")", Balance: spend}, mistralPlanText(modelCount, input, output, cached), true
}

func mistralSpendModelGroups(raw mistralBillingResponse) []map[string]mistralModelUsageData {
	var groups []map[string]mistralModelUsageData
	for _, category := range []*mistralModelUsageCategory{raw.Completion, raw.OCR, raw.Connectors, raw.Audio} {
		if category != nil && len(category.Models) > 0 {
			groups = append(groups, category.Models)
		}
	}
	if raw.LibrariesAPI != nil {
		if raw.LibrariesAPI.Pages != nil && len(raw.LibrariesAPI.Pages.Models) > 0 {
			groups = append(groups, raw.LibrariesAPI.Pages.Models)
		}
		if raw.LibrariesAPI.Tokens != nil && len(raw.LibrariesAPI.Tokens.Models) > 0 {
			groups = append(groups, raw.LibrariesAPI.Tokens.Models)
		}
	}
	if raw.FineTuning != nil {
		if len(raw.FineTuning.Training) > 0 {
			groups = append(groups, raw.FineTuning.Training)
		}
		if len(raw.FineTuning.Storage) > 0 {
			groups = append(groups, raw.FineTuning.Storage)
		}
	}
	return groups
}

func mistralPriceIndex(prices []mistralPrice) map[string]float64 {
	index := map[string]float64{}
	for _, price := range prices {
		if price.BillingMetric == "" || price.BillingGroup == "" || price.Price == "" {
			continue
		}
		value, err := strconv.ParseFloat(price.Price, 64)
		if err != nil {
			continue
		}
		index[price.BillingMetric+"::"+price.BillingGroup] = value
	}
	return index
}

func aggregateMistralModel(model mistralModelUsageData, prices map[string]float64) (input, output, cached int, cost float64) {
	add := func(entries []mistralUsageEntry) (int, float64) {
		var tokens int
		var total float64
		for _, entry := range entries {
			value := entry.Value
			if entry.ValuePaid != nil {
				value = *entry.ValuePaid
			}
			tokens += value
			if entry.BillingMetric != "" && entry.BillingGroup != "" {
				total += float64(value) * prices[entry.BillingMetric+"::"+entry.BillingGroup]
			}
		}
		return tokens, total
	}
	var c float64
	input, c = add(model.Input)
	cost += c
	output, c = add(model.Output)
	cost += c
	cached, c = add(model.Cached)
	cost += c
	return input, output, cached, cost
}

func mistralPlanText(modelCount, input, output, cached int) string {
	if modelCount == 0 && input == 0 && output == 0 && cached == 0 {
		return "Vibe monthly quota"
	}
	return fmt.Sprintf("%d models · %s in / %s out / %s cached", modelCount, compactUsageNumber(input), compactUsageNumber(output), compactUsageNumber(cached))
}

func compactUsageNumber(value int) string {
	abs := value
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs >= 1_000_000_000:
		return trimCompactFloat(float64(value)/1_000_000_000) + "B"
	case abs >= 1_000_000:
		return trimCompactFloat(float64(value)/1_000_000) + "M"
	case abs >= 1_000:
		return trimCompactFloat(float64(value)/1_000) + "k"
	default:
		return strconv.Itoa(value)
	}
}

func trimCompactFloat(value float64) string {
	text := fmt.Sprintf("%.1f", value)
	return strings.TrimSuffix(strings.TrimSuffix(text, "0"), ".")
}

func mistralStatus(status Status, message string, detail *ErrorDetail) Snapshot {
	return Snapshot{Provider: ProviderMistral, Status: status, Error: message, ErrorDetail: detail}
}

func mergeErrorDetails(base, extra *ErrorDetail) *ErrorDetail {
	if extra == nil || len(extra.Fields) == 0 {
		return base
	}
	if base == nil {
		base = &ErrorDetail{Fields: map[string]string{}}
	}
	if base.Fields == nil {
		base.Fields = map[string]string{}
	}
	for key, value := range extra.Fields {
		base.Fields[key] = value
	}
	return base
}
