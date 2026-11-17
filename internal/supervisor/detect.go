package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/cmdprobe"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/telemetry"
	"github.com/hrubymar10/aimebu/internal/vibeconfig"
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
	{"opencode", "opencode", false}, {"opencode", "opencode-docker", true},
	{"vibe", "vibe", false}, {"vibe", "vibe-docker", true},
}

const (
	detectModelsTimeout = 20 * time.Second
	detectOutputLimit   = 8 << 20
	detectNoteMax       = 280
	detectStderrKeep    = 2 << 10
	detectStderrLines   = 3
	detectStderrChars   = 400
	detectStaleRunAge   = time.Hour
	// detectContainerVersionWait is how long one container launcher gets, for all the folders it is asked from together, to give its version while the harnesses are listed, and
	// detectContainerVersionBudget how long all of them get together.
	detectContainerVersionWait   = 5 * time.Second
	detectContainerVersionBudget = 20 * time.Second
)

func (c *Client) detectResult(ctx context.Context, frame protocol.Frame) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.DetectResult, ReqID: frame.ReqID}
	if frame.Detect == nil {
		result.Code, result.Message = "invalid_request", "missing detect request"
		return result
	}
	switch frame.Detect.Kind {
	case "provider_keys":
		answer := c.providerKeysAnswer(*frame.Detect)
		answer.ReqID = frame.ReqID
		return answer
	case "provider_import":
		answer := c.providerImportAnswer()
		answer.ReqID = frame.ReqID
		return answer
	case "harnesses":
		result.HarnessCandidates = detectHarnessesIn(ctx, c.probeDirs())
	case "models":
		dir := frame.Detect.Dir
		temporary := dir == ""
		if dir != "" {
			if err := usableDetectDir(dir); err != nil {
				result.Code, result.Message = "invalid_dir", err.Error()
				return result
			}
			if err := c.protectChosenDetectDir(dir); err != nil {
				result.Code, result.Message = "invalid_dir", err.Error()
				return result
			}
		} else {
			var err error
			dir, err = c.detectDefaultDir()
			if err != nil {
				result.Code, result.Message = "detect_failed", err.Error()
				return result
			}
		}
		var remove func(string) error
		if temporary {
			result.DetectDir = filepath.Dir(dir)
			remove = os.RemoveAll
		} else {
			result.DetectDir = dir
		}
		extras := detectExtras{providers: frame.Detect.Providers, refs: c.launcherRow(frame.Detect.LauncherID).EnvRefs}
		models, err := detectModelsInRunWith(ctx, frame.Detect.Family, frame.Detect.Command, frame.Detect.Args, dir, frame.Detect.Containerized, remove, &extras)
		// The endpoints that could not be asked are reported with a failure as much as with an answer: when the listing found nothing and every
		// endpoint failed, they are the owner's only clue.
		if len(extras.problems) > 0 {
			result.Details, _ = json.Marshal(protocol.ProviderModelsAnswer{EndpointProblems: extras.problems})
		}
		if err != nil {
			result.Code, result.Message = "detect_failed", fmt.Sprintf("model detection in %s: %v", dir, err)
			return result
		}
		result.ModelCandidates = models
	default:
		result.Code, result.Message = "invalid_request", "unknown detect kind"
	}
	return result
}

func detectModelsInRun(ctx context.Context, family, command string, args []string, dir string, containerized bool, remove func(string) error) ([]protocol.ModelCandidate, error) {
	return detectModelsInRunWith(ctx, family, command, args, dir, containerized, remove, nil)
}

// detectExtras is what an OpenCode listing needs beyond the command: the harness's custom providers (defined in the private config the
// listing runs in, and each one's own endpoint is asked), and the launcher's environment references for their keys. problems collects what
// the endpoints that did not answer said.
type detectExtras struct {
	providers []adapter.CustomProvider
	refs      map[string]string
	problems  map[string]string
}

func detectModelsInRunWith(ctx context.Context, family, command string, args []string, dir string, containerized bool, remove func(string) error, extras *detectExtras) ([]protocol.ModelCandidate, error) {
	models, err := detectModelsWith(ctx, family, command, args, dir, containerized, extras)
	if remove != nil {
		if cleanupErr := remove(dir); cleanupErr != nil {
			log.Printf("model detection: remove run folder %s: %v", dir, cleanupErr)
			telemetry.ReportWarning(ctx, "supervisor", fmt.Errorf("model detection: remove run folder: %w", cleanupErr))
		}
	}
	return models, err
}

// detectHarnesses looks for the known harness executables on PATH and in the places a user-level install puts them, and reports each
// once (symlinks resolved). Nothing is saved.
func detectHarnesses(ctx context.Context) []protocol.HarnessCandidate {
	return detectHarnessesIn(ctx, nil)
}

// detectHarnessesIn is detectHarnesses with the folders a container launcher may be asked its version from (the machine's project folders,
// tried in turn): without any, or when none is mounted, the candidate says why it has no version instead of showing nothing.
func detectHarnessesIn(ctx context.Context, probeDirs []string) []protocol.HarnessCandidate {
	dirs := searchDirs(ctx)
	// Asking the container launchers must not make the whole list wait: they share one budget, and a launcher that is not asked in time says so.
	budget, cancelBudget := context.WithTimeout(ctx, detectContainerVersionBudget)
	defer cancelBudget()
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
			} else if len(probeDirs) == 0 {
				candidate.VersionProblem = "no project folder is set on this machine yet, and a container launcher can only be asked from a folder it mounts"
			} else {
				candidate.Version, candidate.VersionProblem = probeLauncherVersionIn(budget, protocol.LauncherDefinition{Tool: item.family, Command: abs, Containerized: true}, abs, probeDirs, detectContainerVersionWait)
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
	// TestMain restricts PATH to fixture tools. Do not bypass it with the
	// normal machine-wide install directories or an npm prefix probe.
	if os.Getenv("AIMEBU_TEST_RESTRICT_PATH") == "1" {
		return dirs
	}
	dirs = append(dirs, "/usr/local/bin", "/opt/homebrew/bin")
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"), filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, ".bun", "bin"), filepath.Join(home, ".opencode", "bin"))
	}
	if npm, err := exec.LookPath("npm"); err == nil {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if out, err := cmdprobe.Output(exec.CommandContext(probe, npm, "prefix", "-g")); err == nil { // npm is a node script: its helpers must not keep this waiting
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
	out, err := cmdprobe.Output(exec.CommandContext(probe, path, "--version"))
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

// usableDetectDir checks a folder the person chose to run model detection in: absolute, and an existing directory on this machine.
func usableDetectDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("the folder to run the command in must be an absolute path: %s", dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("the folder to run the command in does not exist on this machine: %s", dir)
	}
	return nil
}

// detectDefaultDir keeps model detection out of the config root, which contains credentials. Each run gets its own private folder,
// allowing concurrent probes and commands that write into their working directory. The caller removes the run folder after detection.
func (c *Client) detectDefaultDir() (string, error) {
	c.detectMu.Lock()
	defer c.detectMu.Unlock()
	if c.ConfigDir == "" {
		return "", errors.New("the machine's config directory is not known")
	}
	root, err := filepath.Abs(c.ConfigDir)
	if err != nil {
		return "", fmt.Errorf("resolve the machine's config directory: %w", err)
	}
	if err := usableDetectDir(root); err != nil {
		return "", err
	}
	base := filepath.Join(root, "detect")
	if err := os.Mkdir(base, 0700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create the model detection folder %s: %w", base, err)
	}
	info, err := os.Lstat(base)
	if err != nil {
		return "", fmt.Errorf("inspect the model detection folder %s: %w", base, err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("the model detection folder %s must be a private directory", base)
	}
	pruneStaleDetectRuns(base, time.Now())
	dir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return "", fmt.Errorf("create a model detection run folder in %s: %w", base, err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("secure the model detection run folder %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aimebu-detect-run"), nil, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("mark the model detection run folder %s: %w", dir, err)
	}
	return dir, nil
}

// A person may explicitly reuse a run folder left by a failed cleanup. Retire our marker before using it, so a later default probe
// cannot prune the person's chosen folder. Only a direct child of this machine's detect directory can carry our marker.
func (c *Client) protectChosenDetectDir(dir string) error {
	c.detectMu.Lock()
	defer c.detectMu.Unlock()
	if c.ConfigDir == "" {
		return nil
	}
	base, err := filepath.EvalSymlinks(filepath.Join(c.ConfigDir, "detect"))
	if err != nil {
		return nil // A default probe cannot prune this directory either.
	}
	chosen, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve the chosen folder: %w", err)
	}
	if filepath.Dir(chosen) != base || !strings.HasPrefix(filepath.Base(chosen), "run-") {
		return nil
	}
	marker := filepath.Join(chosen, ".aimebu-detect-run")
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("protect the chosen folder %s from cleanup: %w", chosen, err)
	}
	return nil
}

// pruneStaleDetectRuns removes only old marked run folders created by model detection. A matching owner-chosen folder has no marker
// and must survive. A live probe lasts at most 20 seconds, so the one-hour grace leaves concurrent probes alone. Cleanup is advisory.
func pruneStaleDetectRuns(base string, now time.Time) {
	entries, err := os.ReadDir(base)
	if err != nil {
		log.Printf("model detection: list stale run folders in %s: %v", base, err)
		telemetry.ReportWarning(context.Background(), "supervisor", fmt.Errorf("model detection: list stale run folders: %w", err))
		return
	}
	cutoff := now.Add(-detectStaleRunAge)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "run-") || len(entry.Name()) == len("run-") || !entry.IsDir() {
			continue
		}
		path := filepath.Join(base, entry.Name())
		marker, err := os.Lstat(filepath.Join(path, ".aimebu-detect-run"))
		if err != nil || !marker.Mode().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			log.Printf("model detection: inspect run folder %s: %v", path, err)
			telemetry.ReportWarning(context.Background(), "supervisor", fmt.Errorf("model detection: inspect run folder: %w", err))
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			log.Printf("model detection: remove stale run folder %s: %v", path, err)
			telemetry.ReportWarning(context.Background(), "supervisor", fmt.Errorf("model detection: remove stale run folder: %w", err))
		}
	}
}

// detectModels asks one harness's own executable which models it offers, with the command line each family has for that. Claude has no
// such command; its models are looked up by the server. launcherArgs are the launcher's own arguments, put before the listing ones; dir,
// when set, is the working directory. When the command fails, the end of what it wrote to stderr is part of the error, so the person
// sees the harness's own reason rather than a bare exit status.
func detectModels(ctx context.Context, family, command string, launcherArgs []string, dir string) ([]protocol.ModelCandidate, error) {
	return detectModelsFor(ctx, family, command, launcherArgs, dir, false)
}

// detectModelsFor is detectModels for a harness that runs natively (containerized false) or in a container, which changes which models can
// be used: see ModelCandidate.Unreachable.
func detectModelsFor(ctx context.Context, family, command string, launcherArgs []string, dir string, containerized bool) ([]protocol.ModelCandidate, error) {
	return detectModelsWith(ctx, family, command, launcherArgs, dir, containerized, nil)
}

func detectModelsWith(ctx context.Context, family, command string, launcherArgs []string, dir string, containerized bool, extras *detectExtras) ([]protocol.ModelCandidate, error) {
	if err := executablePath(command); err != nil {
		return nil, fmt.Errorf("the harness command is not usable: %w", err)
	}
	for _, arg := range launcherArgs {
		if strings.ContainsRune(arg, '\x00') {
			return nil, errors.New("a harness argument contains NUL")
		}
	}
	if family == "vibe" {
		// Vibe has no command that lists its models: the ones its config defines are what it offers.
		return detectVibeModels(containerized)
	}
	var listArgs []string
	var parse func([]byte) ([]protocol.ModelCandidate, error)
	switch family {
	case "pi":
		listArgs, parse = []string{"--offline", "--list-models"}, parsePiModels
	case "opencode":
		listArgs, parse = []string{"models"}, parseOpencodeModels
	case "codex":
		listArgs, parse = []string{"debug", "models", "--bundled"}, parseCodexModels
	default:
		return nil, errors.New("this family has no model listing command")
	}
	args := append(append([]string{}, launcherArgs...), listArgs...)
	run, cancel := context.WithTimeout(ctx, detectModelsTimeout)
	defer cancel()
	cmd := exec.CommandContext(run, command, args...)
	cmd.Dir = dir
	// A Docker launcher puts its built-in flags (OpenCode's --auto, for one) before the arguments; the listing command must see none of them.
	cmd.Env = append(os.Environ(), strings.ToUpper(family)+"_LAUNCH_FLAGS=")
	if family == "opencode" {
		// The listing runs in the private config an agent turn gets, so it shows what an agent can use, not what the person's own OpenCode
		// config happens to define.
		var custom []adapter.CustomProvider
		if extras != nil {
			custom = extras.providers
		}
		private, cleanup, err := opencodeListingEnv(dir, containerized, custom)
		defer cleanup()
		if err != nil {
			return nil, fmt.Errorf("the harness's private configuration could not be prepared: %w", err)
		}
		cmd.Env = append(cmd.Env, private...)
	}
	var out bytes.Buffer
	stderr := &tailWriter{keep: detectStderrKeep}
	cmd.Stdout = &limitWriter{buf: &out, left: detectOutputLimit}
	cmd.Stderr = stderr
	custom := family == "opencode" && extras != nil && len(extras.providers) > 0
	var models []protocol.ModelCandidate
	var err error
	if runErr := cmdprobe.Run(cmd); runErr != nil {
		if run.Err() != nil {
			return nil, errors.New("the harness did not answer in time")
		}
		// Keep line boundaries for login diagnostics; lastLines joins them for
		// display and may put unrelated prose before the meaningful line.
		if reportsNoModels(string(stderr.buf)) || reportsNoModels(out.String()) {
			// OpenCode's own list being empty says nothing about a custom provider's endpoint, which is still asked below.
			if !custom {
				return nil, noModelsError(family)
			}
			err = noModelsError(family)
		} else if said := stderr.lastLines(detectStderrLines, detectStderrChars); said != "" {
			return nil, fmt.Errorf("the harness could not list its models (%v): %s", runErr, said)
		} else {
			return nil, fmt.Errorf("the harness could not list its models: %w", runErr)
		}
	} else {
		models, err = parse(out.Bytes())
	}
	if custom {
		// Config-listed models remain usable when an endpoint cannot answer. The
		// private command may omit them, so include them explicitly.
		have := map[string]bool{}
		for _, model := range models {
			have[model.Slug] = true
		}
		for _, provider := range extras.providers {
			for _, model := range provider.Models {
				slug := provider.ID + "/" + model.ID
				if !modelID.MatchString(slug) || have[slug] {
					continue
				}
				models = append(models, protocol.ModelCandidate{Slug: slug, Label: model.Name})
				have[slug] = true
			}
		}
		// Also ask each endpoint with its own key, if set here.
		listed, problems := providerEndpointModels(ctx, extras.providers, extras.refs)
		extras.problems = problems
		if len(models) > 0 || len(listed) > 0 {
			if err != nil {
				err = nil
			}
			for _, model := range listed {
				if !have[model.Slug] {
					models = append(models, model)
					have[model.Slug] = true
				}
			}
		}
	}
	return models, err
}

// tailWriter keeps only the last keep bytes written to it.
type tailWriter struct {
	buf  []byte
	keep int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if extra := len(w.buf) - w.keep; extra > 0 {
		w.buf = append(w.buf[:0], w.buf[extra:]...)
	}
	return len(p), nil
}

// lastLines is the last few non-empty lines kept, joined by spaces and cut to at most chars runes, valid UTF-8 and without control
// characters (terminal colour codes included).
func (w *tailWriter) lastLines(lines, chars int) string {
	text := strings.ToValidUTF8(string(w.buf), "")
	var kept []string
	all := strings.Split(text, "\n")
	for i := len(all) - 1; i >= 0 && len(kept) < lines; i-- {
		if line := cleanStderrLine(all[i]); line != "" {
			kept = append([]string{line}, kept...)
		}
	}
	said := strings.Join(kept, " ")
	if utf8.RuneCountInString(said) > chars {
		said = "…" + string([]rune(said)[utf8.RuneCountInString(said)-chars+1:])
	}
	return said
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func cleanStderrLine(line string) string {
	line = ansiEscape.ReplaceAllString(line, "")
	line = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
	return strings.TrimSpace(line)
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
var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)
var modelProvider = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]*$`)
var modelSize = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?[KMGTP]?$`)

func noModelsError(family string) error {
	return fmt.Errorf("%s reports no models — is it logged in? Run /login in it", family)
}

func reportsNoModels(output string) bool {
	for _, raw := range strings.Split(output, "\n") {
		line := strings.ToLower(cleanStderrLine(raw))
		for _, prefix := range []string{"error:", "warning:", "fatal:"} {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				break
			}
		}
		if strings.HasPrefix(line, "no models available") || strings.HasPrefix(line, "no models found") ||
			strings.HasPrefix(line, "not logged in") || strings.HasPrefix(line, "use /login") ||
			strings.HasPrefix(line, "please run /login") {
			return true
		}
	}
	return false
}

func parsePiModels(out []byte) ([]protocol.ModelCandidate, error) {
	models := []protocol.ModelCandidate{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 || !modelProvider.MatchString(fields[0]) || !modelID.MatchString(fields[0]+"/"+fields[1]) ||
			!modelSize.MatchString(fields[2]) || !modelSize.MatchString(fields[3]) ||
			(fields[4] != "yes" && fields[4] != "no") || (fields[5] != "yes" && fields[5] != "no") {
			continue
		}
		model := protocol.ModelCandidate{Slug: fields[0] + "/" + fields[1], Label: fields[1]}
		model.Note = fmt.Sprintf("context %s, max output %s, thinking %s, images %s", fields[2], fields[3], fields[4], fields[5])
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, noModelsError("pi")
	}
	return models, nil
}

// vibeDefaultModel is the model Vibe 2.25.0 runs when its config names none, offered even when the config defines no models of its own.
var vibeDefaultModel = vibeconfig.Model{Alias: "mistral-medium-3.5", DisplayName: "Mistral Medium 3.5", Provider: "mistral"}

// detectVibeModels lists the models of the person's Vibe config (config.toml in $VIBE_HOME, ~/.vibe by default) by alias, which is what an
// agent is started with, and Vibe's built-in default. Only the config's models and providers tables are read, never its .env.
func detectVibeModels(containerized bool) ([]protocol.ModelCandidate, error) {
	home := os.Getenv("VIBE_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("the Vibe home directory is not known")
		}
		home = filepath.Join(user, ".vibe")
	}
	blocks, err := vibeconfig.Read(filepath.Join(home, "config.toml"))
	if err != nil {
		return nil, fmt.Errorf("the Vibe config could not be read: %w", err)
	}
	listed := vibeconfig.Models(blocks)
	seen := map[string]bool{}
	for _, model := range listed {
		seen[model.Alias] = true
	}
	if !seen[vibeDefaultModel.Alias] {
		listed = append(listed, vibeDefaultModel)
	}
	models := []protocol.ModelCandidate{}
	providers := vibeconfig.ProviderBases(blocks)
	for _, model := range listed {
		// A model with no display name is labelled by what it is, the model and its provider, and not only by the alias the config gave it:
		// "local" says nothing about what runs where.
		label := model.DisplayName
		if label == "" {
			label = model.Alias
			if model.Name != "" && model.Name != model.Alias {
				label = model.Name
			}
			if model.Provider != "" {
				label += " (" + model.Provider + ")"
			}
		}
		candidate := protocol.ModelCandidate{Slug: model.Alias, Label: label}
		if containerized {
			if base := providers[model.Provider]; loopbackBase(base) {
				candidate.Unreachable = "its provider is at " + base + ", the machine's own loopback address, which a container cannot reach"
			}
		}
		var note []string
		if model.Provider != "" {
			note = append(note, "provider "+model.Provider)
		}
		if model.Name != "" && model.Name != model.Alias {
			note = append(note, "model "+model.Name)
		}
		if model.InputPrice != "" || model.OutputPrice != "" {
			note = append(note, fmt.Sprintf("$%s in / $%s out per million tokens", orZero(model.InputPrice), orZero(model.OutputPrice)))
		}
		candidate.Note = strings.Join(note, ", ")
		models = append(models, candidate)
	}
	return models, nil
}

// loopbackBase says whether a provider's API base URL names the machine itself.
func loopbackBase(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

func orZero(price string) string {
	if price == "" {
		return "0"
	}
	return price
}

// parseOpencodeModels reads `opencode models`: one provider/model per line.
func parseOpencodeModels(out []byte) ([]protocol.ModelCandidate, error) {
	models := []protocol.ModelCandidate{}
	for _, line := range strings.Split(string(out), "\n") {
		slug := strings.TrimSpace(line)
		if strings.EqualFold(slug, "use/login") {
			continue
		}
		provider, name, ok := strings.Cut(slug, "/")
		if !ok || !modelProvider.MatchString(provider) || !modelID.MatchString(slug) || name == "" {
			continue
		}
		models = append(models, protocol.ModelCandidate{Slug: slug, Label: name})
	}
	if len(models) == 0 {
		return nil, noModelsError("opencode")
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
		if reportsNoModels(string(out)) || len(strings.TrimSpace(string(out))) == 0 {
			return nil, noModelsError("codex")
		}
		return nil, errors.New("the harness's model list could not be read")
	}
	models := []protocol.ModelCandidate{}
	for _, item := range catalog.Models {
		if item.Visibility != "list" || !modelID.MatchString(item.Slug) {
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
		return nil, noModelsError("codex")
	}
	return models, nil
}

// DetectHarnesses is detectHarnesses for callers outside the supervisor, such as `aimebu doctor --machine`: the harness executables found on
// this machine, each reported once, nothing saved.
func DetectHarnesses(ctx context.Context) []protocol.HarnessCandidate { return detectHarnesses(ctx) }
