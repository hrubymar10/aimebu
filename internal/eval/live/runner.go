package live

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

// Target is one model on one launcher: a column of the report.
type Target struct {
	Name       string `json:"name"`        // the column label, a plain word
	Tool       string `json:"tool"`        // claude, codex, pi, opencode or vibe
	LauncherID string `json:"launcher_id"` // the id of a row of the launchers file
	Model      string `json:"model"`
	Provider   string `json:"provider,omitempty"`
	// Providers are the custom providers the model may need (OpenCode); see providersFor.
	Providers []adapter.CustomProvider `json:"providers,omitempty"`
}

// ErrStopped says the run ended early on purpose: the budget, the quota guard or a cancel.
var ErrStopped = errors.New("run stopped")

// Options is one run.
type Options struct {
	RunID string
	// ScratchDir is a folder the launchers mount (their visible roots); each target gets a checkout named aimebu-eval-<RunID>-<target> in it, which
	// is what marks the Claude transcripts of the run (Claude names a session's folder after the working directory).
	ScratchDir string
	Targets    []Target
	// Launchers are the launcher rows the targets run through; they come from the owner (docs/eval.md), never from the supervisor's own files.
	Launchers []protocol.LauncherRow
	Scenarios []eval.Scenario
	Reps      int
	// MaxUSD stops the run once the cost the harnesses report (an estimate for a subscription) passes it; 0 means no cap.
	MaxUSD float64
	// Quota is read before the run, every QuotaEvery cases and after it. A weekly window above StopAbovePct stops the run.
	Quota         *QuotaSource
	QuotaEvery    int
	StopAbovePct  float64
	TurnTimeout   time.Duration
	VisibleHost   string // how a container reaches this machine, when it is not host.docker.internal
	MCP           *MCPServer
	Log           io.Writer
	NewAdapter    func(tool string) (adapter.Adapter, error)
	Preflight     func(tool, checkout, launcher string) error
	QuotaBefore   Usage
	QuotaAfter    Usage
	spentUSD      float64
	turnCounter   int
	preflightDone map[string]bool
}

// Run runs every scenario of every target Reps times, with repairs, and returns the cases it finished. When the run is stopped on purpose it
// returns the cases so far and an error wrapping ErrStopped.
func (o *Options) Run(ctx context.Context) ([]eval.Case, error) {
	cases, err := o.run(ctx)
	o.finalQuota()
	return cases, err
}

// finalQuota is the reading the report's after column shows, taken on every way out of a run: after a clean run and after one that was
// stopped (the budget, the guard, an unreadable quota, a cancel). The run's own context may be cancelled by then, so it has a short one of its
// own. When the reading fails, the last successful one stands.
func (o *Options) finalQuota() {
	if o.Quota == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if usage, err := o.Quota.Fetch(ctx); err == nil {
		o.QuotaAfter = usage
	}
}

func (o *Options) run(ctx context.Context) ([]eval.Case, error) {
	if o.Reps < 1 || len(o.Targets) == 0 || len(o.Scenarios) == 0 || o.MCP == nil || o.NewAdapter == nil {
		return nil, errors.New("a run needs targets, scenarios, reps >= 1, an MCP stand-in and an adapter source")
	}
	if o.TurnTimeout == 0 {
		o.TurnTimeout = 8 * time.Minute
	}
	if o.QuotaEvery == 0 {
		o.QuotaEvery = 10
	}
	if o.StopAbovePct == 0 {
		o.StopAbovePct = 90
	}
	o.preflightDone = map[string]bool{}
	rows := map[string]protocol.LauncherRow{}
	for _, row := range o.Launchers {
		rows[row.ID] = row
	}
	if o.Quota != nil {
		reading, err := o.Quota.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading the quota before the run: %w", err)
		}
		usage := reading.Usage
		o.QuotaBefore, o.QuotaAfter = usage, usage
		if over := usage.OverLimitFor(o.StopAbovePct, o.targetTools(), reading.Active); over != "" {
			return nil, fmt.Errorf("%w before it started: %s", ErrStopped, over)
		}
	}
	var cases []eval.Case
	count := 0
	for _, target := range o.Targets {
		row, ok := rows[target.LauncherID]
		if !ok || !row.Containerized {
			return cases, fmt.Errorf("target %s: launcher %q is not in the launchers file or is not a Docker launcher (the eval runs through the containers agents run in)", target.Name, target.LauncherID)
		}
		if err := builtinProviderKey(target, row); err != nil {
			return cases, fmt.Errorf("target %s: %w", target.Name, err)
		}
		impl, err := o.NewAdapter(target.Tool)
		if err != nil {
			return cases, err
		}
		checkout, err := o.checkout(target)
		if err != nil {
			return cases, err
		}
		if o.Preflight != nil && !o.preflightDone[target.Name] {
			if err := o.Preflight(target.Tool, checkout, row.ResolvedCommand); err != nil {
				return cases, fmt.Errorf("target %s: %w", target.Name, err)
			}
			o.preflightDone[target.Name] = true
		}
		for _, scenario := range o.Scenarios {
			if len(scenario.OnlyKinds) > 0 && !contains(scenario.OnlyKinds, target.Tool) {
				continue
			}
			for rep := 0; rep < o.Reps; rep++ {
				if err := ctx.Err(); err != nil {
					return cases, fmt.Errorf("%w: %v", ErrStopped, err)
				}
				if o.MaxUSD > 0 && o.spentUSD >= o.MaxUSD {
					return cases, fmt.Errorf("%w: the reported cost passed the cap of $%.2f", ErrStopped, o.MaxUSD)
				}
				if o.Quota != nil && count > 0 && count%o.QuotaEvery == 0 {
					reading, err := o.Quota.Read(ctx)
					if err != nil {
						return cases, fmt.Errorf("%w: the quota cannot be read: %v", ErrStopped, err)
					}
					usage := reading.Usage
					o.QuotaAfter = usage
					if over := usage.OverLimitFor(o.StopAbovePct, o.targetTools(), reading.Active); over != "" {
						return cases, fmt.Errorf("%w: %s", ErrStopped, over)
					}
				}
				c := o.runCase(ctx, impl, target, row, checkout, scenario, rep)
				cases = append(cases, c)
				count++
				fmt.Fprintf(o.log(), "%s %s rep %d: %s\n", target.Name, scenario.ID, rep, summary(c))
			}
		}
	}
	return cases, nil
}

// targetTools are the /api/usages provider names of the run's targets (providers.ProviderFor, the server's own mapping): the only
// providers whose quota windows the guard looks at. A target with no provider there (neither Target.Provider nor Target.Model names a usages provider; both reach the turn spec at runcase.go:102) adds none.
func (o *Options) targetTools() map[string]bool {
	tools := map[string]bool{}
	for _, t := range o.Targets {
		if name, ok := providers.ProviderFor(t.Tool, t.Model, t.Provider); ok {
			tools[name] = true
		}
	}
	return tools
}

// Spent is the cost the harnesses reported so far, in dollars.
func (o *Options) Spent() float64 { return o.spentUSD }

func (o *Options) log() io.Writer {
	if o.Log == nil {
		return io.Discard
	}
	return o.Log
}

// checkout makes the target's scratch checkout, a git repository named for the run.
func (o *Options) checkout(target Target) (string, error) {
	root, err := filepath.EvalSymlinks(o.ScratchDir)
	if err != nil {
		return "", fmt.Errorf("scratch dir: %w", err)
	}
	dir := filepath.Join(root, "aimebu-eval-"+o.RunID+"-"+target.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if out, err := gitCommand(dir, "init", "-q").CombinedOutput(); err != nil {
			return "", fmt.Errorf("git init: %v: %s", err, out)
		}
	}
	if err := ensureScratchExclude(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// uuid is a random version 4 UUID, the session id Claude and Pi take for a first turn.
func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func summary(c eval.Case) string {
	out := ""
	for i, a := range c.Attempts {
		if i > 0 {
			out += " -> "
		}
		switch {
		case a.Class == eval.ClassHarness:
			out += "harness failure"
		case a.Correct:
			out += "correct"
		case a.Valid:
			out += "valid but wrong"
		default:
			out += "invalid (" + a.Class + ")"
		}
	}
	return out
}

// withIdentity puts the target's harness and model into the context's you_are and its roster entry, the way a real delivery names them.
func withIdentity(context json.RawMessage, target Target) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(context, &fields); err != nil {
		return nil, err
	}
	set := func(entry map[string]any) {
		if entry["id"] == eval.WorkerID {
			entry["harness"], entry["model"] = target.Tool, target.Model
		}
	}
	var you map[string]any
	if json.Unmarshal(fields["you_are"], &you) == nil {
		set(you)
		fields["you_are"], _ = json.Marshal(you)
	}
	var roster []map[string]any
	if json.Unmarshal(fields["team_roster"], &roster) == nil {
		for _, entry := range roster {
			set(entry)
		}
		fields["team_roster"], _ = json.Marshal(roster)
	}
	return json.Marshal(fields)
}

// heldNote is the system note a repair turn carries, in the words the server uses when it holds a reply (internal/store/answers.go): the error
// only, since the agent has its own answer in its session.
func heldNote(noteNo int, reason string) json.RawMessage {
	note := map[string]any{"note_id": fmt.Sprintf("note_01EVALHELD%04d", noteNo), "room_id": eval.TaskRoom, "from": "system",
		"message_id": fmt.Sprintf("msg_01EVALHELD%04d", noteNo), "no": noteNo, "body": "Your previous reply was held: " + reason, "why": "your reply was held"}
	out, _ := json.Marshal([]any{note})
	return out
}

// builtinProviderKey refuses an OpenCode target on one of OpenCode's own providers (a "<provider>/" model that is not one of the target's custom
// providers) when this process has none of the keys such a provider reads (adapter.KnownProviderKeys). The container is handed only the
// variables this process has, as for a native turn, so without one OpenCode fails in about two seconds with "UnknownError: Unexpected server
// error", the same text it prints for an unknown model, which is what every case of an OpenCode target did in run 3. The supervisor of a real
// agent has the owner's keys in its environment; the eval's process may not.
func builtinProviderKey(target Target, row protocol.LauncherRow) error {
	if target.Tool != eval.KindOpenCode || !strings.Contains(target.Model, "/") || len(providersFor(target)) > 0 {
		return nil
	}
	for _, name := range adapter.KnownProviderKeys {
		if adapter.EnvValue(name, row.EnvRefs) != "" {
			return nil
		}
	}
	return fmt.Errorf("model %s is on one of OpenCode's own providers, which read their key from the environment of this process, and none of %s is set here; export the provider's key (OLLAMA_API_KEY for ollama-cloud), or give the model a custom provider, or leave the target out",
		target.Model, strings.Join(adapter.KnownProviderKeys, ", "))
}
