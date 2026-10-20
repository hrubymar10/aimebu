package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

// The harnesses the adapters know, by family, with the executable names they are installed under. The Docker wrappers have their own names
// and no version probe here (they need a mirrored working directory).
var detectNames = []struct {
	family, name string
	docker       bool
}{
	{"claude", "claude", false}, {"claude", "claude-docker", true},
	{"codex", "codex", false}, {"codex", "codex-docker", true},
	{"pi", "pi", false}, {"pi", "pi-docker", true},
	{"opencode", "opencode", false},
}

const (
	detectModelsTimeout = 20 * time.Second
	detectOutputLimit   = 8 << 20
	detectNoteMax       = 280
)

func (c *Client) detectResult(ctx context.Context, frame protocol.Frame) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.DetectResult, ReqID: frame.ReqID}
	if frame.Detect == nil {
		result.Code, result.Message = "invalid_request", "missing detect request"
		return result
	}
	switch frame.Detect.Kind {
	case "harnesses":
		result.HarnessCandidates = detectHarnesses(ctx)
	case "models":
		models, err := detectModels(ctx, frame.Detect.Family, frame.Detect.Command)
		if err != nil {
			result.Code, result.Message = "detect_failed", err.Error()
			return result
		}
		result.ModelCandidates = models
	default:
		result.Code, result.Message = "invalid_request", "unknown detect kind"
	}
	return result
}

// detectHarnesses looks for the known harness executables on PATH and in the places a user-level install puts them, and reports each
// once (symlinks resolved). Nothing is saved.
func detectHarnesses(ctx context.Context) []protocol.HarnessCandidate {
	dirs := searchDirs(ctx)
	seen := map[string]bool{}
	found := []protocol.HarnessCandidate{}
	for _, item := range detectNames {
		for _, dir := range dirs {
			abs, err := filepath.Abs(filepath.Join(dir, item.name))
			if err != nil || executablePath(abs) != nil {
				continue
			}
			canonical, err := filepath.EvalSymlinks(abs)
			if err != nil || seen[canonical] {
				continue
			}
			seen[canonical] = true
			candidate := protocol.HarnessCandidate{Family: item.family, Command: abs, Containerized: item.docker}
			if !item.docker {
				candidate.Version = commandVersion(ctx, abs)
			}
			found = append(found, candidate)
		}
	}
	return found
}

// searchDirs are the directories searched: every absolute entry of PATH (all of them, so a second install further down PATH is found
// too, not only the first one a shell would run), then user-level install locations and the global npm prefix.
func searchDirs(ctx context.Context) []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(dir) {
			dirs = append(dirs, dir)
		}
	}
	dirs = append(dirs, "/usr/local/bin", "/opt/homebrew/bin")
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"), filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, ".bun", "bin"), filepath.Join(home, ".opencode", "bin"))
	}
	if npm, err := exec.LookPath("npm"); err == nil {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(probe, npm, "prefix", "-g").Output(); err == nil {
			if prefix := strings.TrimSpace(string(out)); filepath.IsAbs(prefix) {
				dirs = append(dirs, filepath.Join(prefix, "bin"))
			}
		}
	}
	return dirs
}

func commandVersion(ctx context.Context, path string) string {
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probe, path, "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

// detectModels asks one harness's own executable which models it offers, with the command line each family has for that. Claude has no
// such command; its models are looked up by the server.
func detectModels(ctx context.Context, family, command string) ([]protocol.ModelCandidate, error) {
	if err := executablePath(command); err != nil {
		return nil, fmt.Errorf("the harness command is not usable: %w", err)
	}
	var args []string
	var parse func([]byte) ([]protocol.ModelCandidate, error)
	switch family {
	case "pi":
		args, parse = []string{"--offline", "--list-models"}, parsePiModels
	case "opencode":
		args, parse = []string{"models"}, parseOpencodeModels
	case "codex":
		args, parse = []string{"debug", "models", "--bundled"}, parseCodexModels
	default:
		return nil, errors.New("this family has no model listing command")
	}
	run, cancel := context.WithTimeout(ctx, detectModelsTimeout)
	defer cancel()
	cmd := exec.CommandContext(run, command, args...)
	var out bytes.Buffer
	cmd.Stdout = &limitWriter{buf: &out, left: detectOutputLimit}
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if run.Err() != nil {
			return nil, errors.New("the harness did not answer in time")
		}
		return nil, fmt.Errorf("the harness could not list its models: %w", err)
	}
	return parse(out.Bytes())
}

type limitWriter struct {
	buf  *bytes.Buffer
	left int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errors.New("output too large")
	}
	w.left -= len(p)
	return w.buf.Write(p)
}

// parsePiModels reads `pi --offline --list-models`: a header, then one row per model with provider, model, context, max output,
// thinking and images columns. The slug is provider/model.
func parsePiModels(out []byte) ([]protocol.ModelCandidate, error) {
	models := []protocol.ModelCandidate{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "provider" && fields[1] == "model" {
			continue
		}
		model := protocol.ModelCandidate{Slug: fields[0] + "/" + fields[1], Label: fields[1]}
		if len(fields) >= 6 {
			model.Note = fmt.Sprintf("context %s, max output %s, thinking %s, images %s", fields[2], fields[3], fields[4], fields[5])
		}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, errors.New("the harness listed no models")
	}
	return models, nil
}

// parseOpencodeModels reads `opencode models`: one provider/model per line.
func parseOpencodeModels(out []byte) ([]protocol.ModelCandidate, error) {
	models := []protocol.ModelCandidate{}
	for _, line := range strings.Split(string(out), "\n") {
		slug := strings.TrimSpace(line)
		provider, name, ok := strings.Cut(slug, "/")
		if !ok || provider == "" || name == "" || strings.ContainsAny(slug, " \t") {
			continue
		}
		models = append(models, protocol.ModelCandidate{Slug: slug, Label: name})
	}
	if len(models) == 0 {
		return nil, errors.New("the harness listed no models")
	}
	return models, nil
}

// parseCodexModels reads `codex debug models --bundled`: a JSON catalog; the models with visibility "list" are the ones Codex shows.
func parseCodexModels(out []byte) ([]protocol.ModelCandidate, error) {
	var catalog struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out, &catalog); err != nil {
		return nil, errors.New("the harness's model list could not be read")
	}
	models := []protocol.ModelCandidate{}
	for _, item := range catalog.Models {
		if item.Visibility != "list" || item.Slug == "" {
			continue
		}
		label := item.DisplayName
		if label == "" {
			label = item.Slug
		}
		note := strings.TrimSpace(item.Description)
		if utf8.RuneCountInString(note) > detectNoteMax {
			note = string([]rune(note)[:detectNoteMax-1]) + "…"
		}
		models = append(models, protocol.ModelCandidate{Slug: item.Slug, Label: label, Note: note})
	}
	if len(models) == 0 {
		return nil, errors.New("the harness listed no models")
	}
	return models, nil
}
