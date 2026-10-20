package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

const (
	claudeModelsURL     = "https://raw.githubusercontent.com/anthropics/anthropic-sdk-go/main/message.go"
	claudeModelsTimeout = 5 * time.Second
	claudeModelsLimit   = 4 << 20
	detectHarnessWait   = 40 * time.Second
	detectModelsWait    = 30 * time.Second
	detectFailedHint    = "Add the model ID manually."
)

// detectedHarness is a candidate the owner may tick; nothing is saved until the owner adds it.
type detectedHarness struct {
	protocol.HarnessCandidate
	Label        string `json:"label"`
	AlreadyAdded bool   `json:"already_added"`
}

// detectedModel is a candidate model; nothing is saved until the owner adds it.
type detectedModel struct {
	protocol.ModelCandidate
	AlreadyListed bool `json:"already_listed"`
}

func (s *Server) requestDetect(ctx context.Context, conn *machineConn, request protocol.DetectRequest, wait time.Duration) (protocol.Frame, error) {
	reqID := fmt.Sprintf("detect-%d", time.Now().UnixNano())
	result := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	conn.launcherPending[reqID] = result
	conn.harnessMu.Unlock()
	defer func() { conn.harnessMu.Lock(); delete(conn.launcherPending, reqID); conn.harnessMu.Unlock() }()
	if err := conn.send(protocol.Frame{V: protocol.Version, Type: protocol.Detect, ReqID: reqID, Detect: &request}); err != nil {
		return protocol.Frame{}, err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case answer := <-result:
		return answer, nil
	case <-conn.done:
		return protocol.Frame{}, errors.New("machine disconnected")
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	case <-timer.C:
		return protocol.Frame{}, errors.New("the supervisor did not answer in time")
	}
}

// detectConn finds the connected supervisor of a machine that can look around, or answers why not.
func (s *Server) detectConn(w http.ResponseWriter, machineID string) (*machineConn, bool) {
	conn, err := s.machineForID(machineID)
	if err != nil {
		apiError(w, 503, "machine_unavailable", "the machine is not connected")
		return nil, false
	}
	if !conn.supportsDetect {
		apiError(w, 503, "machine_unavailable", "the machine needs an updated supervisor to auto-detect")
		return nil, false
	}
	return conn, true
}

// machineHarnessDetect lists the harnesses installed on a machine, as candidates the owner ticks. Nothing is saved.
func (s *Server) machineHarnessDetect(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	machineID := r.PathValue("id")
	if _, err := s.Store.MachineByID(r.Context(), machineID); errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return
	} else if err != nil {
		fail(w, err)
		return
	}
	conn, ok := s.detectConn(w, machineID)
	if !ok {
		return
	}
	answer, err := s.requestDetect(r.Context(), conn, protocol.DetectRequest{Kind: "harnesses"}, detectHarnessWait)
	if err != nil {
		apiError(w, 502, "detect_failed", "Couldn't detect harnesses: "+err.Error())
		return
	}
	if answer.Code != "" {
		apiError(w, 502, "detect_failed", "Couldn't detect harnesses: "+answer.Message)
		return
	}
	existing, err := s.Store.MachineLaunchers(r.Context(), machineID)
	if err != nil {
		fail(w, err)
		return
	}
	candidates := make([]detectedHarness, 0, len(answer.HarnessCandidates))
	for _, candidate := range answer.HarnessCandidates {
		item := detectedHarness{HarnessCandidate: candidate, Label: defaultHarnessLabel(candidate.Family, candidate.Containerized)}
		for _, launcher := range existing {
			if launcher.Tool == candidate.Family && launcher.Containerized == candidate.Containerized && (launcher.Command == candidate.Command || launcher.Command == filepath.Base(candidate.Command)) {
				item.AlreadyAdded = true
			}
		}
		candidates = append(candidates, item)
	}
	writeJSON(w, 200, map[string]any{"candidates": candidates})
}

// machineHarnessModelsDetect lists the models a harness offers, as candidates the owner ticks. Claude's come from the Anthropic SDK's
// published source (fetched only now, on the owner's click, with a built-in list as the fallback); the others are asked of the harness
// itself on its machine. Nothing is saved.
func (s *Server) machineHarnessModelsDetect(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	machineID, launcherID := r.PathValue("id"), r.PathValue("launcher")
	harness, err := s.Store.LauncherByID(r.Context(), machineID, launcherID)
	if err != nil {
		s.launcherMutationError(w, err)
		return
	}
	listed, err := s.Store.HarnessModels(r.Context(), launcherID)
	if err != nil {
		fail(w, err)
		return
	}
	var found []protocol.ModelCandidate
	source := ""
	switch {
	case harness.Tool == "claude":
		found, source = s.claudeModelCandidates(r.Context())
	case harness.Containerized:
		apiError(w, 400, "detect_unsupported", "Auto-detect asks the harness's own command, which a Docker wrapper cannot answer here. Add the model ID manually.")
		return
	default:
		conn, ok := s.detectConn(w, machineID)
		if !ok {
			return
		}
		conn.harnessMu.Lock()
		resolved := ""
		for _, row := range conn.launchers {
			if row.ID == launcherID {
				resolved = row.ResolvedCommand
			}
		}
		conn.harnessMu.Unlock()
		if resolved == "" {
			apiError(w, 409, "launcher_unavailable", "The harness was not found on the machine. "+detectFailedHint)
			return
		}
		answer, err := s.requestDetect(r.Context(), conn, protocol.DetectRequest{Kind: "models", Family: harness.Tool, Command: resolved}, detectModelsWait)
		if err != nil {
			apiError(w, 502, "detect_failed", "Couldn't detect the models: "+err.Error()+". "+detectFailedHint)
			return
		}
		if answer.Code != "" {
			apiError(w, 502, "detect_failed", "Couldn't detect the models: "+answer.Message+". "+detectFailedHint)
			return
		}
		found, source = answer.ModelCandidates, "harness"
	}
	have := map[string]bool{}
	for _, model := range listed {
		have[model.Slug] = true
	}
	candidates := []detectedModel{}
	seen := map[string]bool{}
	for _, candidate := range found {
		candidate = cleanModelCandidate(harness.Tool, candidate)
		if candidate.Slug == "" || seen[candidate.Slug] {
			continue
		}
		seen[candidate.Slug] = true
		candidates = append(candidates, detectedModel{ModelCandidate: candidate, AlreadyListed: have[candidate.Slug]})
	}
	if len(candidates) == 0 {
		apiError(w, 502, "detect_failed", "Couldn't detect the models: nothing usable was listed. "+detectFailedHint)
		return
	}
	writeJSON(w, 200, map[string]any{"candidates": candidates, "source": source})
}

// cleanModelCandidate drops what the model list would refuse (a bad id) and trims what it limits (label and note length).
func cleanModelCandidate(family string, model protocol.ModelCandidate) protocol.ModelCandidate {
	if !store.ValidHarnessModelSlug(model.Slug) || family == "opencode" && !strings.Contains(model.Slug, "/") {
		return protocol.ModelCandidate{}
	}
	if model.Label == "" {
		model.Label = model.Slug
	}
	if utf8.RuneCountInString(model.Label) > 80 {
		model.Label = string([]rune(model.Label)[:80])
	}
	if utf8.RuneCountInString(model.Note) > 280 {
		model.Note = string([]rune(model.Note)[:279]) + "…"
	}
	return model
}

// ---- Claude's models, from the Anthropic SDK source ----

// claudeModelCache keeps the last good parse and the ETag it came with, so a repeat click costs a conditional request.
type claudeModelCache struct {
	mu     sync.Mutex
	etag   string
	models []protocol.ModelCandidate
}

var claudeConstRE = regexp.MustCompile(`^\s*(\w+)\s+Model\s*=\s*"([^"]+)"`)
var claudeDatedRE = regexp.MustCompile(`-\d{8}$`)

// parseAnthropicModels reads the model constants of the Anthropic Go SDK's message.go: current models only (not deprecated, not a dated
// snapshot, not a restricted-access one), each with the SDK's own doc comment as its note.
func parseAnthropicModels(source []byte) []protocol.ModelCandidate {
	var models []protocol.ModelCandidate
	var comment []string
	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			if text := strings.TrimSpace(strings.TrimPrefix(trimmed, "//")); text != "" {
				comment = append(comment, text)
			}
			continue
		}
		match := claudeConstRE.FindStringSubmatch(line)
		if match != nil {
			slug := match[2]
			doc := strings.Join(comment, " ")
			if strings.HasPrefix(slug, "claude-") && !strings.Contains(doc, "Deprecated:") && !claudeDatedRE.MatchString(slug) && !strings.Contains(slug, "mythos") {
				models = append(models, protocol.ModelCandidate{Slug: slug, Label: claudeLabel(slug), Note: doc})
			}
		}
		comment = comment[:0]
	}
	return models
}

// claudeLabel makes "claude-haiku-5-5" read "Claude Haiku 5.5".
func claudeLabel(slug string) string {
	var words []string
	for _, part := range strings.Split(slug, "-") {
		if part == "" {
			continue
		}
		if len(words) > 0 && isDigits(part) && isDigits(words[len(words)-1]) {
			words[len(words)-1] += "." + part
			continue
		}
		words = append(words, strings.ToUpper(part[:1])+part[1:])
	}
	return strings.Join(words, " ")
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// claudeModelSnapshot is the list used when the SDK source cannot be fetched; it is the current list of the SDK when this was written.
var claudeModelSnapshot = []protocol.ModelCandidate{
	{Slug: "claude-haiku-5-5", Label: "Claude Haiku 5.5", Note: "Fastest model for high-volume, real-time tasks"},
	{Slug: "claude-sonnet-5-5", Label: "Claude Sonnet 5.5", Note: "Efficient model for coding and agents"},
	{Slug: "claude-fable-5-1", Label: "Claude Fable 5.1", Note: "Frontier intelligence for ambitious tasks across coding, scientific discovery, and enterprise workflows"},
	{Slug: "claude-opus-5-5", Label: "Claude Opus 5.5", Note: "Powerful intelligence for coding, knowledge work, and long-running agents"},
	{Slug: "claude-sonnet-5", Label: "Claude Sonnet 5", Note: "Efficient model for coding and agents"},
	{Slug: "claude-fable-5", Label: "Claude Fable 5", Note: "Next generation of intelligence for the hardest knowledge work and coding problems"},
	{Slug: "claude-opus-5", Label: "Claude Opus 5", Note: "Powerful intelligence for long-running agents and coding"},
	{Slug: "claude-opus-4-8", Label: "Claude Opus 4.8", Note: "Powerful intelligence for long-running agents and coding"},
	{Slug: "claude-opus-4-7", Label: "Claude Opus 4.7", Note: "Powerful intelligence for long-running agents and coding"},
	{Slug: "claude-opus-4-6", Label: "Claude Opus 4.6", Note: "Powerful intelligence for long-running agents and coding"},
	{Slug: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6", Note: "Best combination of speed and intelligence"},
	{Slug: "claude-haiku-4-5", Label: "Claude Haiku 4.5"},
	{Slug: "claude-opus-4-5", Label: "Claude Opus 4.5", Note: "Powerful intelligence for long-running agents and coding"},
}

// claudeModelCandidates returns Claude's models and where they came from: "github" (fetched), "cache" (unchanged since the last fetch) or
// "snapshot" (the fetch failed, so the list built into this server).
func (s *Server) claudeModelCandidates(ctx context.Context) ([]protocol.ModelCandidate, string) {
	cache := &s.claudeModels
	cache.mu.Lock()
	defer cache.mu.Unlock()
	fetch := s.claudeModelsFetch
	if fetch == nil {
		fetch = fetchClaudeModelSource
	}
	etag := ""
	if len(cache.models) > 0 {
		etag = cache.etag
	}
	body, newETag, notModified, err := fetch(ctx, etag)
	switch {
	case err != nil:
		if len(cache.models) > 0 {
			return cache.models, "cache"
		}
		return claudeModelSnapshot, "snapshot"
	case notModified && len(cache.models) > 0:
		return cache.models, "cache"
	}
	models := parseAnthropicModels(body)
	if len(models) == 0 {
		return claudeModelSnapshot, "snapshot"
	}
	cache.etag, cache.models = newETag, models
	return models, "github"
}

// fetchClaudeModelSource gets the SDK source: five seconds, stdlib only, conditional on the last ETag.
func fetchClaudeModelSource(ctx context.Context, etag string) ([]byte, string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, claudeModelsTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeModelsURL, nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, "", false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return nil, etag, true, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("the SDK source answered %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, claudeModelsLimit))
	return body, response.Header.Get("ETag"), false, err
}
