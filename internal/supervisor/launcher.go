package supervisor

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/cmdprobe"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

var launcherEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateLauncher(def protocol.LauncherDefinition) error {
	if def.ID == "" || def.Name == "" || (def.Tool != "claude" && def.Tool != "codex" && def.Tool != "pi" && def.Tool != "opencode" && def.Tool != "vibe") {
		return errors.New("launcher ID, name, and supported tool required")
	}
	if def.Command == "" || strings.ContainsAny(def.Command, "\x00\n\r") || strings.TrimSpace(def.Command) != def.Command || strings.Contains(def.Command, " ") {
		return errors.New("command must be one executable name or absolute path, without arguments")
	}
	if strings.ContainsRune(def.Command, filepath.Separator) && !filepath.IsAbs(def.Command) {
		return errors.New("command path must be absolute")
	}
	if def.Containerized && def.Tool == "pi" && def.Toolbox {
		return errors.New("Pi container launchers cannot provide the toolbox with the current adapter")
	}
	if def.AgentServerURL != "" {
		u, err := url.Parse(def.AgentServerURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return errors.New("agent server URL must be a plain http(s) origin")
		}
	}
	for _, arg := range def.Args {
		if strings.ContainsRune(arg, '\x00') {
			return errors.New("launcher argument contains NUL")
		}
	}
	for target, source := range def.EnvRefs {
		if !launcherEnvName.MatchString(target) || !launcherEnvName.MatchString(source) {
			return errors.New("environment references must be variable names")
		}
	}
	for _, root := range def.VisibleRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return errors.New("visible roots must be canonical absolute paths")
		}
		if canonical, err := filepath.EvalSymlinks(root); err != nil || canonical != root {
			return fmt.Errorf("visible root is not canonical: %s", root)
		}
	}
	for _, mapping := range def.PathMaps {
		if !filepath.IsAbs(mapping.Host) || !filepath.IsAbs(mapping.Visible) ||
			filepath.Clean(mapping.Host) != mapping.Host || filepath.Clean(mapping.Visible) != mapping.Visible {
			return errors.New("path mappings need absolute clean prefixes")
		}
	}
	return nil
}

// inspectLauncher makes the row of a launcher. checkout is the folder a container launcher is first asked from; more are the other project
// folders of the machine, tried in turn when the first is one its container does not mount.
func inspectLauncher(def protocol.LauncherDefinition, checkout string, more ...string) (protocol.LauncherRow, error) {
	row := protocol.LauncherRow{LauncherDefinition: def}
	dirs := probeFolders(checkout, more)
	if err := validateLauncher(def); err != nil {
		return row, err
	}
	path, err := exec.LookPath(def.Command)
	if err != nil {
		row.UnavailableReason = "command not found on this machine"
		return row, nil
	}
	if path, err = filepath.Abs(path); err != nil {
		return row, err
	}
	if err := executablePath(path); err != nil {
		return row, err
	}
	row.ResolvedCommand = path
	if len(dirs) > 0 || !def.Containerized {
		version, problem := probeLauncherVersionIn(context.Background(), def, path, dirs, versionProbeTimeout)
		row.Version = version
		if problem != "" {
			if def.Containerized {
				row.VersionProblem = problem
			} else {
				row.UnavailableReason = "launcher version check failed"
			}
		}
	} else {
		row.VersionProblem = "no project folder is set on this machine yet, and a container launcher can only be asked from a folder it mounts"
	}
	// The feature probe needs no mounted project folder: without one it runs in
	// a fresh empty folder of ours. Codex's probe does ask the Docker daemon for
	// its actual home, with a separate deadline. Other probes return promptly.
	// A launcher too old to know the probe dies on its mount check there.
	if def.Containerized && (adapter.DockerNeedsForwardEnv(def.Tool) || def.Tool == "claude") {
		probeLauncherFeatures(&row, def, path, checkout)
	}
	if !def.Enabled {
		row.UnavailableReason = "disabled by owner"
	} else if row.UnavailableReason == "" {
		row.Available = true
	}
	return row, nil
}

// versionProbeTimeout is how long the launcher gets to answer --version when its row is made; versionReprobeTimeout is how long a container
// launcher that did not answer then gets once the connection is up (starting a container can take longer than the first look waits).
const (
	versionProbeTimeout   = 3 * time.Second
	versionReprobeTimeout = 20 * time.Second
	versionTimedOut       = "did not answer --version in time"
)

// probeLauncherVersion runs the launcher with --version in dir (empty for the supervisor's own) and returns its answer, or why there is none:
// what it printed on stderr (a Docker launcher says "Directory … is not mounted in the harness container" there), or that it timed out.
func probeLauncherVersion(def protocol.LauncherDefinition, path, dir string, timeout time.Duration) (version, problem string) {
	return probeLauncherVersionContext(context.Background(), def, path, dir, timeout)
}

// probeFolders is the folders a container launcher is asked from: first, then more, each once, none empty.
func probeFolders(first string, more []string) []string {
	var dirs []string
	seen := map[string]bool{}
	for _, dir := range append([]string{first}, more...) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// probeLauncherVersionIn asks the launcher for its version from each folder in turn and stops at the first answer. A container launcher
// only runs in a folder its container mounts and the server cannot tell which of a machine's project folders that is, so one that is
// not mounted must not hide the version: only when none answers is the problem returned, and then the most useful one (what the launcher
// said that is not just "not mounted", else that no project folder of this machine is mounted). No folder at all is a launcher that runs
// anywhere (dirs empty: the supervisor's own folder).
func probeLauncherVersionIn(parent context.Context, def protocol.LauncherDefinition, path string, dirs []string, timeout time.Duration) (version, problem string) {
	if len(dirs) == 0 {
		return probeLauncherVersionContext(parent, def, path, "", timeout)
	}
	// One deadline for the whole launcher, however many folders there are, and the first timeout ends it: a launcher that hangs is hanging
	// for every folder, and the hello loop asks launchers one after another before it reads any frame, so the time a launcher may take is
	// what the caller gave it, not that times the number of folders.
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	notMounted, other := 0, ""
	for _, dir := range dirs {
		version, problem = probeLauncherVersionContext(ctx, def, path, dir, timeout)
		if version != "" {
			return version, ""
		}
		if ctx.Err() != nil || problem == versionTimedOut {
			return "", versionTimedOut
		}
		if strings.Contains(problem, "not mounted") {
			notMounted++
		} else if other == "" {
			other = problem
		}
	}
	if other != "" {
		return "", other
	}
	if notMounted > 1 {
		return "", fmt.Sprintf("none of this machine's %d project folders is mounted in the harness container", notMounted)
	}
	return "", problem
}

func probeLauncherVersionContext(parent context.Context, def protocol.LauncherDefinition, path, dir string, timeout time.Duration) (version, problem string) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	args := append(append([]string{}, def.Args...), "--version")
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = launcherEnvironment(def.EnvRefs, def.Tool)
	output, err := cmdprobe.Output(cmd) // bounded: a helper the launcher left behind cannot hold the answer up
	if err == nil {
		return strings.TrimSpace(string(output)), ""
	}
	if ctx.Err() != nil {
		return "", versionTimedOut
	}
	var failed *exec.ExitError
	if errors.As(err, &failed) {
		if said := adapter.SanitizeStderr(string(failed.Stderr)); said != "" {
			return "", said
		}
	}
	if said := adapter.SanitizeStderr(string(output)); said != "" {
		return "", said
	}
	return "", err.Error()
}

func (c *Client) hasUnknownContainerLauncherLocked() bool {
	if c.harnessProbeDir == "" && len(c.harnessProbeMore) == 0 {
		return false
	}
	for _, row := range c.Launchers {
		if row.Containerized && row.ResolvedCommand != "" && row.Version == "" && row.VersionProblem != "" {
			return true
		}
	}
	return false
}

// A connection has at most one retry loop. New launcher definitions can start it again after all unknown versions were resolved.
func (c *Client) startLauncherReprobe(ctx context.Context) {
	c.mu.Lock()
	if ctx.Err() != nil || c.reprobeRunning || !c.hasUnknownContainerLauncherLocked() {
		c.mu.Unlock()
		return
	}
	c.reprobeRunning = true
	c.reprobeDone = make(chan struct{})
	done := c.reprobeDone
	interval := c.launcherReprobeInterval
	c.mu.Unlock()
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		defer func() {
			c.mu.Lock()
			c.reprobeRunning = false
			restart := ctx.Err() == nil && c.hasUnknownContainerLauncherLocked()
			close(done)
			c.mu.Unlock()
			if restart {
				c.startLauncherReprobe(ctx)
			}
		}()
		// A timeout gets its existing prompt retry; other failures wait for a later tick.
		c.reprobeUnknownLaunchers(ctx, true)
		for {
			c.mu.Lock()
			pending := c.hasUnknownContainerLauncherLocked()
			c.mu.Unlock()
			if !pending || ctx.Err() != nil {
				return
			}
			jitter := time.Duration(rand.Int63n(int64(interval/5) + 1))
			timer := time.NewTimer(interval + jitter)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			c.reprobeUnknownLaunchers(ctx, false)
		}
	}()
}

// Recheck unresolved container versions sequentially and report only a changed row.
func (c *Client) reprobeUnknownLaunchers(ctx context.Context, timedOutOnly bool) {
	c.mu.Lock()
	dirs := probeFolders(c.harnessProbeDir, c.harnessProbeMore)
	var pending []protocol.LauncherRow
	for _, row := range c.Launchers {
		if row.Containerized && row.ResolvedCommand != "" && row.Version == "" && row.VersionProblem != "" && (!timedOutOnly || row.VersionProblem == versionTimedOut) {
			pending = append(pending, row)
		}
	}
	c.mu.Unlock()
	if len(dirs) == 0 {
		return
	}
	changed := false
	for _, row := range pending {
		if ctx.Err() != nil {
			return
		}
		version, problem := probeLauncherVersionIn(ctx, row.LauncherDefinition, row.ResolvedCommand, dirs, versionReprobeTimeout)
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		for i := range c.Launchers {
			if c.Launchers[i].ID == row.ID && c.Launchers[i].Version == "" && c.Launchers[i].VersionProblem == row.VersionProblem && reflect.DeepEqual(c.Launchers[i].LauncherDefinition, row.LauncherDefinition) && (version != "" || problem != row.VersionProblem) {
				c.Launchers[i].Version, c.Launchers[i].VersionProblem = version, problem
				changed = true
			}
		}
		c.mu.Unlock()
	}
	if changed {
		c.refreshCapabilities()
		_ = c.sendCurrent(ctx, protocol.Frame{V: protocol.Version, Type: protocol.LauncherStatus, Launchers: c.launcherRows(), Capabilities: c.capabilityReportsSnapshot()})
	}
}

// probeLauncherFeatures asks once per launcher row and records features and
// Codex's home, or why the answer is unavailable. Doctor and Settings read
// that row instead of probing on every turn.
func probeLauncherFeatures(row *protocol.LauncherRow, def protocol.LauncherDefinition, path, checkout string) {
	timeout := 3 * time.Second
	if def.Tool == "codex" {
		timeout = 10 * time.Second // Codex's probe waits for the Docker daemon.
	}
	probeLauncherFeaturesWithTimeout(row, def, path, checkout, timeout)
}

func probeLauncherFeaturesWithTimeout(row *protocol.LauncherRow, def protocol.LauncherDefinition, path, checkout string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	private := checkout == ""
	if private {
		empty, err := os.MkdirTemp("", "aimebu-feature-probe-")
		if err != nil {
			row.FeatureProblem = "the launcher's feature probe could not run: " + err.Error()
			return
		}
		defer os.RemoveAll(empty)
		checkout = empty
	}
	report, err := adapter.DockerProbe(ctx, path, def.Args, checkout, launcherEnvironment(def.EnvRefs, def.Tool))
	if err != nil {
		if !adapter.DockerNeedsForwardEnv(def.Tool) {
			return // the tool needs no feature: an answer that cannot be read only means none is known
		}
		if private && strings.Contains(err.Error(), "not mounted") {
			// Asked from a folder of ours, a launcher that predates the probe checks the mount before anything else and says so: that is
			// the launcher being old, not a failure of this one.
			row.FeatureProblem = "launcher too old: it does not answer the feature probe, which " + def.Tool + " needs (update harness-docker)"
			return
		}
		row.FeatureProblem = "the launcher's feature probe failed: " + err.Error()
		return
	}
	row.CodexHome = report.CodexHome
	for feature := range report.Features {
		row.Features = append(row.Features, feature)
	}
	sort.Strings(row.Features)
	// A tool that is handed a variable by name (the agent's key, or its configuration) needs forward_env; Claude asks for the features as well to know
	// whether its sessions are shared with the host.
	if adapter.DockerNeedsForwardEnv(def.Tool) && !report.Features[adapter.FeatureForwardEnv] {
		row.FeatureProblem = "launcher too old: it does not forward environment variables, which " + def.Tool + " needs (update harness-docker)"
	} else if def.Tool == "codex" && report.CodexHome == "" {
		row.FeatureProblem = codexHomeMissingProblem
	}
}

func launcherEnvironment(refs map[string]string, tool string) []string {
	env := append([]string(nil), os.Environ()...)
	// Existing wrapper flags must not smuggle settings into a launched turn.
	env = append(env, strings.ToUpper(tool)+"_LAUNCH_FLAGS=")
	for target, source := range refs {
		env = append(env, target+"="+os.Getenv(source))
	}
	return env
}

func withinLauncherRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func launcherCheckout(def protocol.LauncherDefinition, hostPath string) (string, error) {
	canonical, err := filepath.EvalSymlinks(hostPath)
	if err != nil || canonical != hostPath {
		return "", errors.New("checkout path must be canonical on this machine")
	}
	if len(def.VisibleRoots) > 0 {
		visible := false
		for _, root := range def.VisibleRoots {
			if withinLauncherRoot(root, canonical) {
				visible = true
				break
			}
		}
		if !visible {
			return "", errors.New("checkout is outside launcher visible roots")
		}
	}
	best := ""
	translated := canonical
	for _, mapping := range def.PathMaps {
		if withinLauncherRoot(mapping.Host, canonical) && len(mapping.Host) > len(best) {
			rel, err := filepath.Rel(mapping.Host, canonical)
			if err != nil {
				return "", err
			}
			best = mapping.Host
			translated = filepath.Join(mapping.Visible, rel)
		}
	}
	return translated, nil
}

func preflightLauncher(ctx context.Context, def protocol.LauncherDefinition, row protocol.LauncherRow, checkout string) (string, error) {
	visible, err := launcherCheckout(def, checkout)
	if err != nil {
		return "", err
	}
	if !row.Available {
		return "", fmt.Errorf("launcher unavailable: %s", row.UnavailableReason)
	}
	timeout := 10 * time.Second
	if def.Containerized {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(append([]string{}, def.Args...), "--version")
	cmd := exec.CommandContext(ctx, row.ResolvedCommand, args...)
	cmd.Dir = checkout
	cmd.Env = launcherEnvironment(def.EnvRefs, def.Tool)
	// A child may keep the output pipe open after the version command exits.
	// Bound that wait, including after cancellation of the command.
	cmd.WaitDelay = time.Second
	if def.Containerized && adapter.DockerNeedsForwardEnv(def.Tool) {
		// A launcher that cannot forward the environment would start the harness without its isolated configuration and toolbox: say so now.
		if err := adapter.RequireDockerFeature(ctx, def.Tool, row.ResolvedCommand, def.Args, checkout, launcherEnvironment(def.EnvRefs, def.Tool), adapter.FeatureForwardEnv); err != nil {
			return "", err
		}
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil {
			return visible, nil // The version command exited successfully.
		}
		return "", fmt.Errorf("launcher checkout preflight: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return visible, nil
}

// probeDirs are the folders a container launcher is asked from: the first one known to work (a spawned checkout), then the machine's
// project folders.
func (c *Client) probeDirs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return probeFolders(c.harnessProbeDir, c.harnessProbeMore)
}

func (c *Client) setLauncher(def protocol.LauncherDefinition) error {
	dirs := c.probeDirs()
	var first string
	var more []string
	if len(dirs) > 0 {
		first, more = dirs[0], dirs[1:]
	}
	row, err := inspectLauncher(def, first, more...)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.codexHomeRetries == nil {
		c.codexHomeRetries = make(map[string]codexHomeRetry)
	}
	retry := c.codexHomeRetries[def.ID]
	retry.generation++
	retry.next = time.Time{}
	c.codexHomeRetries[def.ID] = retry
	for i := range c.Launchers {
		if c.Launchers[i].ID == def.ID {
			c.Launchers[i] = row
			return nil
		}
	}
	c.Launchers = append(c.Launchers, row)
	return nil
}

const codexHomeRetryInterval = 2 * time.Minute
const codexHomeMissingProblem = "Codex warm: the launcher has not reported codex_home; the container may not be running yet or harness-docker may be too old"

type codexHomeRetry struct {
	next       time.Time
	generation uint64
}

// A missing Docker home may mean the container was stopped at hello. A turn
// stays cold while a bounded background probe refreshes the launcher's row.
func (c *Client) maybeReprobeCodexHome(row protocol.LauncherRow) {
	if row.Tool != "codex" || !row.Containerized || row.CodexHome != "" || row.ResolvedCommand == "" {
		return
	}
	c.mu.Lock()
	if c.Warm.Idle <= 0 || !c.codexWarmEnabled {
		c.mu.Unlock()
		return
	}
	if c.codexHomeRetries == nil {
		c.codexHomeRetries = make(map[string]codexHomeRetry)
	}
	retry := c.codexHomeRetries[row.ID]
	if time.Now().Before(retry.next) {
		c.mu.Unlock()
		return
	}
	retry.next = time.Now().Add(codexHomeRetryInterval)
	c.codexHomeRetries[row.ID] = retry
	checkout := c.harnessProbeDir
	c.mu.Unlock()

	go func() {
		fresh := protocol.LauncherRow{LauncherDefinition: row.LauncherDefinition}
		probeLauncherFeatures(&fresh, row.LauncherDefinition, row.ResolvedCommand, checkout)
		if fresh.CodexHome == "" {
			return
		}
		c.mu.Lock()
		if c.codexHomeRetries[row.ID].generation != retry.generation {
			c.mu.Unlock()
			return
		}
		updated := false
		for i := range c.Launchers {
			current := &c.Launchers[i]
			if current.ID == row.ID && current.CodexHome == "" && current.ResolvedCommand == row.ResolvedCommand && reflect.DeepEqual(current.LauncherDefinition, row.LauncherDefinition) {
				current.CodexHome = fresh.CodexHome
				current.Features = fresh.Features
				current.FeatureProblem = fresh.FeatureProblem
				updated = true
				break
			}
		}
		c.mu.Unlock()
		if updated {
			c.refreshCapabilities()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = c.sendCurrent(ctx, protocol.Frame{V: protocol.Version, Type: protocol.LauncherStatus, Launchers: c.launcherRows(), Capabilities: c.capabilityReportsSnapshot()})
		}
	}()
}

func (c *Client) launcherRow(id string) protocol.LauncherRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, row := range c.Launchers {
		if row.ID == id {
			return row
		}
	}
	return protocol.LauncherRow{}
}

func (c *Client) launcherRows() []protocol.LauncherRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := append([]protocol.LauncherRow(nil), c.Launchers...)
	for i := range rows {
		rows[i] = c.launcherRowForReportLocked(rows[i])
	}
	return rows
}

// The missing home matters only while Codex warm mode is active. Keep the
// raw probe result in the cache so a later opt-in can expose the limitation.
func (c *Client) launcherRowForReport(row protocol.LauncherRow) protocol.LauncherRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.launcherRowForReportLocked(row)
}

func (c *Client) launcherRowForReportLocked(row protocol.LauncherRow) protocol.LauncherRow {
	if row.FeatureProblem == codexHomeMissingProblem && (!c.codexWarmEnabled || c.Warm.Idle <= 0) {
		row.FeatureProblem = ""
	}
	return row
}

func (c *Client) reportLauncherStatus() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.sendCurrent(ctx, protocol.Frame{V: protocol.Version, Type: protocol.LauncherStatus, Launchers: c.launcherRows(), Capabilities: c.capabilityReportsSnapshot()})
}
