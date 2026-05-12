package server

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/goccy/go-json"
)

//go:embed defaults_prompts.json
var defaultPromptCatalogJSON []byte

// promptCatalogEntry is the metadata-only shape from defaults_prompts.json.
type promptCatalogEntry struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Group       string   `json:"group"`
	Description string   `json:"description"`
	Tokens      []string `json:"tokens"`
}

// PromptEntry is the full shape returned by GET /settings/prompts.
type PromptEntry struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Group       string   `json:"group"`
	Description string   `json:"description"`
	Tokens      []string `json:"tokens"`
	Body        string   `json:"body"`
	DefaultBody string   `json:"default_body"`
	Overridden  bool     `json:"overridden"`
}

type promptsEnvelope struct {
	Prompts map[string]string `json:"prompts"`
}

var (
	promptCatalog    []promptCatalogEntry
	promptCatalogSet map[string]bool // key → true, for O(1) validation

	compiledPromptDefaults   map[string]string
	compiledPromptDefaultsMu sync.RWMutex
)

func init() {
	var entries []promptCatalogEntry
	if json.Unmarshal(defaultPromptCatalogJSON, &entries) == nil {
		promptCatalog = entries
		promptCatalogSet = make(map[string]bool, len(entries))
		for _, e := range entries {
			promptCatalogSet[e.Key] = true
		}
	}
}

// Prompts use an "overrides-only" storage model: s.prompts holds only keys
// the user has explicitly changed. getPrompt falls back to the compiled
// default for everything else. This differs from macros (which seed from a
// stored catalog) because prompt defaults live in Go source and the JSON file
// carries metadata only. New catalog keys added in a release appear
// automatically in listings with their compiled default — no migration needed.

// SetPromptDefaults registers the compiled-in default bodies. Must be called
// once before the server begins serving requests.
func SetPromptDefaults(defaults map[string]string) {
	compiledPromptDefaultsMu.Lock()
	compiledPromptDefaults = defaults
	compiledPromptDefaultsMu.Unlock()
}

func compiledDefaultFor(key string) string {
	compiledPromptDefaultsMu.RLock()
	v := compiledPromptDefaults[key]
	compiledPromptDefaultsMu.RUnlock()
	return v
}

// ── Store operations ───────────────────────────────────────────────

func (s *store) loadPrompts() {
	data, err := os.ReadFile(filepath.Join(s.dir, "prompts.json"))
	if err != nil {
		return
	}
	var env promptsEnvelope
	if json.Unmarshal(data, &env) != nil || env.Prompts == nil {
		return
	}
	// Drop overrides for keys removed from the catalog (stale from a prior
	// release). getPrompt has no catalog gate, so without this filter a
	// removed key's stored override would be silently served forever.
	clean := make(map[string]string, len(env.Prompts))
	dropped := 0
	for k, v := range env.Prompts {
		if promptCatalogSet[k] {
			clean[k] = v
		} else {
			dropped++
		}
	}
	if dropped > 0 {
		log.Printf("aimebu: loadPrompts: dropped %d stale prompt override(s) for keys no longer in catalog", dropped)
	}
	s.promptsMu.Lock()
	s.prompts = clean
	s.promptsMu.Unlock()
}

func (s *store) savePrompts() {
	s.promptsMu.RLock()
	env := promptsEnvelope{Prompts: s.prompts}
	data, err := json.MarshalIndent(env, "", "  ")
	s.promptsMu.RUnlock()
	if err == nil {
		atomicWrite(filepath.Join(s.dir, "prompts.json"), data)
	}
}

// getPrompt returns the user override if present, else the compiled default.
func (s *store) getPrompt(key string) string {
	s.promptsMu.RLock()
	v, ok := s.prompts[key]
	s.promptsMu.RUnlock()
	if ok {
		return v
	}
	return compiledDefaultFor(key)
}

// setPrompt stores a user override. Returns false if the key is not in the catalog.
func (s *store) setPrompt(key, value string) bool {
	if !promptCatalogSet[key] {
		return false
	}
	s.promptsMu.Lock()
	s.prompts[key] = value
	s.promptsMu.Unlock()
	s.savePrompts()
	return true
}

// deletePrompt removes a user override, reverting to the compiled default.
func (s *store) deletePrompt(key string) {
	s.promptsMu.Lock()
	delete(s.prompts, key)
	s.promptsMu.Unlock()
	s.savePrompts()
}

// deleteAllPrompts removes all user overrides.
func (s *store) deleteAllPrompts() {
	s.promptsMu.Lock()
	s.prompts = make(map[string]string)
	s.promptsMu.Unlock()
	s.savePrompts()
}

// listPrompts returns all catalog entries with their current effective bodies.
func (s *store) listPrompts() []PromptEntry {
	s.promptsMu.RLock()
	overrides := make(map[string]string, len(s.prompts))
	for k, v := range s.prompts {
		overrides[k] = v
	}
	s.promptsMu.RUnlock()

	out := make([]PromptEntry, 0, len(promptCatalog))
	for _, e := range promptCatalog {
		override, overridden := overrides[e.Key]
		body := override
		if !overridden {
			body = compiledDefaultFor(e.Key)
		}
		tokens := e.Tokens
		if tokens == nil {
			tokens = []string{}
		}
		out = append(out, PromptEntry{
			Key:         e.Key,
			Label:       e.Label,
			Group:       e.Group,
			Description: e.Description,
			Tokens:      tokens,
			Body:        body,
			DefaultBody: compiledDefaultFor(e.Key),
			Overridden:  overridden,
		})
	}
	return out
}

// clearPrompts wipes all user overrides (used by prune -a).
func (s *store) clearPrompts() {
	s.promptsMu.Lock()
	s.prompts = make(map[string]string)
	s.promptsMu.Unlock()
	_ = os.Remove(filepath.Join(s.dir, "prompts.json"))
}
