package vibe

import (
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

// sessionStats is the part of a session's meta.json (its "stats") that prices a turn: Vibe totals tokens over the whole session, resumes
// included, and counts cached tokens inside the prompt tokens.
type sessionStats struct {
	PromptTokens     int64   `json:"session_prompt_tokens"`
	CompletionTokens int64   `json:"session_completion_tokens"`
	CachedTokens     int64   `json:"session_cached_tokens"`
	ContextTokens    int64   `json:"context_tokens"`
	Cost             float64 `json:"session_cost"`
	InputPrice       float64 `json:"input_price_per_million"`
	OutputPrice      float64 `json:"output_price_per_million"`
}

// readStats finds the session's directory (named session_<time>_<first eight characters of its id>) and reads its stats. A session that
// was never persisted, or a file that cannot be read, has none.
func readStats(saveDir, sessionID string) *sessionStats {
	short := sessionID
	if len(short) > 8 {
		short = short[:8]
	}
	if short == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(saveDir, "session_*_"+short, "meta.json"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		a, errA := os.Stat(matches[i])
		b, errB := os.Stat(matches[j])
		return errA == nil && (errB != nil || a.ModTime().After(b.ModTime()))
	})
	data, err := os.ReadFile(matches[0])
	if err != nil {
		return nil
	}
	var meta struct {
		SessionID string       `json:"session_id"`
		Stats     sessionStats `json:"stats"`
	}
	if json.Unmarshal(data, &meta) != nil || (meta.SessionID != "" && meta.SessionID != sessionID) {
		return nil
	}
	return &meta.Stats
}

// usage is what this turn alone spent: the session's totals now less what they were before it (nothing, for a first turn). A turn that
// left them unchanged, which is what one killed before Vibe saved the session does, reports nothing. The cost is Vibe's own, cumulative
// over the session (the store takes the difference), and unknown for a model with no price: a free-looking turn would be counted as free.
func (t *turn) usage(sessionID string, terminal bool) *adapter.Usage {
	after := readStats(t.saveDir, sessionID)
	if after == nil {
		return nil
	}
	if t.started && t.before == nil {
		// A resumed session whose totals before the turn cannot be read has nothing to subtract from: counting the session's whole history as
		// this turn's would overcharge every limit, so the tokens stay unknown. The cost is Vibe's cumulative figure, which the store turns
		// into the turn's own, so it still counts.
		log.Printf("vibe: no readable stats for session %s before the resumed turn %s: its token usage is unknown", sessionID, t.id)
		spent := &adapter.Usage{Partial: !terminal}
		if after.ContextTokens > 0 {
			context := after.ContextTokens
			spent.ContextUsed = &context
		}
		if after.InputPrice > 0 || after.OutputPrice > 0 {
			cost := after.Cost
			spent.CostEstimate, spent.CostCumulative = &cost, true
		}
		return spent
	}
	var before sessionStats
	if t.before != nil {
		before = *t.before
	}
	prompt, completion, cached := after.PromptTokens-before.PromptTokens, after.CompletionTokens-before.CompletionTokens, after.CachedTokens-before.CachedTokens
	if prompt <= 0 && completion <= 0 && after.Cost == before.Cost {
		return nil
	}
	cached = max(min(cached, prompt), 0)
	in, out := max(prompt-cached, 0), max(completion, 0)
	spent := &adapter.Usage{TokensIn: &in, TokensOut: &out, TokensCacheRead: &cached, Partial: !terminal}
	if after.ContextTokens > 0 {
		context := after.ContextTokens
		spent.ContextUsed = &context
	}
	if after.InputPrice > 0 || after.OutputPrice > 0 {
		cost := after.Cost
		spent.CostEstimate, spent.CostCumulative = &cost, true
	}
	return spent
}
