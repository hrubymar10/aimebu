package credtarget

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/goccy/go-json"
)

// Registry lists the targets that receive fan-out. The two live files are
// implicit; extra external targets are persisted in
// `<serverDir>/credential-targets.json` (none in P1).
type Registry struct {
	serverDir string
	home      string // resolved home, so tests never touch the real one

	// LivePath resolves a tool's live credential file. The default follows the
	// CLIs: claude honours CLAUDE_CONFIG_DIR, codex honours CODEX_HOME. The
	// server wires it to the switcher's own resolution so both always agree.
	LivePath func(tool string) (string, error)

	mu       sync.Mutex
	external []Target
}

// NewRegistry builds a registry. home is the directory whose .claude/.codex are
// the live dirs.
func NewRegistry(serverDir, home string) *Registry {
	r := &Registry{serverDir: serverDir, home: home}
	r.LivePath = r.defaultLivePath
	return r
}

func (r *Registry) defaultLivePath(tool string) (string, error) {
	switch tool {
	case ToolClaude:
		if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
			return filepath.Join(dir, ".credentials.json"), nil
		}
		return filepath.Join(r.home, ".claude", ".credentials.json"), nil
	case ToolCodex:
		if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
			return filepath.Join(dir, "auth.json"), nil
		}
		return filepath.Join(r.home, ".codex", "auth.json"), nil
	}
	return "", fmt.Errorf("credtarget: unknown tool %q", tool)
}

func (r *Registry) externalPath() string {
	return filepath.Join(r.serverDir, "credential-targets.json")
}

type registryFile struct {
	Version int      `json:"version"`
	Targets []Target `json:"targets"`
}

// Load reads the external targets file (missing = none).
func (r *Registry) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := os.ReadFile(r.externalPath())
	if err != nil {
		if os.IsNotExist(err) {
			r.external = nil
			return nil
		}
		return err
	}
	var file registryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("credtarget: parse %s: %w", r.externalPath(), err)
	}
	var out []Target
	for _, t := range file.Targets {
		if t.ID == "" || t.Path == "" || (t.Tool != ToolClaude && t.Tool != ToolCodex) {
			continue
		}
		t.Role = RoleExternal
		out = append(out, t)
	}
	r.external = out
	return nil
}

// Register adds (or replaces, by id) an external target and persists the file.
func (r *Registry) Register(t Target) error {
	if t.ID == "" || !filepath.IsAbs(t.Path) || (t.Tool != ToolClaude && t.Tool != ToolCodex) {
		return errors.New("credtarget: a target needs an id, an absolute path and a known tool")
	}
	t.Role = RoleExternal
	r.mu.Lock()
	defer r.mu.Unlock()
	replaced := false
	for i := range r.external {
		if r.external[i].ID == t.ID {
			r.external[i], replaced = t, true
		}
	}
	if !replaced {
		r.external = append(r.external, t)
	}
	return r.saveLocked()
}

// Unregister removes an external target by id (missing is not an error).
func (r *Registry) Unregister(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.external[:0]
	for _, t := range r.external {
		if t.ID != id {
			kept = append(kept, t)
		}
	}
	r.external = kept
	return r.saveLocked()
}

func (r *Registry) saveLocked() error {
	data, err := json.MarshalIndent(registryFile{Version: 1, Targets: r.external}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.serverDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.serverDir, ".credential-targets-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.externalPath())
}

func (r *Registry) live() []Target {
	var out []Target
	for _, tool := range []string{ToolClaude, ToolCodex} {
		path, err := r.LivePath(tool)
		if err != nil || path == "" {
			continue
		}
		out = append(out, Target{ID: "live/" + tool, Tool: tool, Path: path, Role: RoleLive})
	}
	return out
}

// Targets returns the live targets for both tools followed by the externals.
func (r *Registry) Targets() []Target {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(r.live(), r.external...)
}

// ForProfile returns the targets that must be rewritten when `profile` of
// `tool` changes: the tool's live target when profile is the active one
// (active=true), plus externals pinned to that profile.
func (r *Registry) ForProfile(tool, profile string, active bool) []Target {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Target
	if active {
		for _, t := range r.live() {
			if t.Tool == tool {
				out = append(out, t)
			}
		}
	}
	for _, t := range r.external {
		if t.Tool == tool && t.Profile == profile {
			out = append(out, t)
		}
	}
	return out
}

// Active returns the live (and unpinned external) targets of tool: what Switch
// rewrites after the active pointer moved.
func (r *Registry) ActiveTargets(tool string) []Target {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Target
	for _, t := range r.live() {
		if t.Tool == tool {
			out = append(out, t)
		}
	}
	for _, t := range r.external {
		if t.Tool == tool && t.Profile == "" {
			out = append(out, t)
		}
	}
	return out
}
