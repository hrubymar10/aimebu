package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

func inspectLauncher(def protocol.LauncherDefinition, checkout string) (protocol.LauncherRow, error) {
	row := protocol.LauncherRow{LauncherDefinition: def}
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
	if checkout != "" || !def.Containerized {
		version, problem := probeLauncherVersion(def, path, checkout, versionProbeTimeout)
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
	// Like the --version probe above, only with a checkout to run in: an old launcher that ignores the probe runs the harness with these
	// arguments, which must not happen in an arbitrary directory, and the hello loop must not wait on every launcher.
	if def.Containerized && checkout != "" && (adapter.DockerNeedsForwardEnv(def.Tool) || def.Tool == "claude") {
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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

// reprobeSlowLaunchers asks again, with more time, the container launchers whose first --version did not answer in time, and reports the
// rows it filled in. A container can take longer to answer than the first look waits, and the connection must not wait on every launcher.
func (c *Client) reprobeSlowLaunchers(ctx context.Context) {
	c.mu.Lock()
	dir := c.harnessProbeDir
	var slow []protocol.LauncherRow
	for _, row := range c.Launchers {
		if row.Containerized && row.VersionProblem == versionTimedOut && row.ResolvedCommand != "" {
			slow = append(slow, row)
		}
	}
	c.mu.Unlock()
	if dir == "" || len(slow) == 0 {
		return
	}
	changed := false
	for _, row := range slow {
		if ctx.Err() != nil {
			return
		}
		version, problem := probeLauncherVersion(row.LauncherDefinition, row.ResolvedCommand, dir, versionReprobeTimeout)
		c.mu.Lock()
		for i := range c.Launchers {
			if c.Launchers[i].ID == row.ID && c.Launchers[i].VersionProblem == versionTimedOut {
				c.Launchers[i].Version, c.Launchers[i].VersionProblem = version, problem
				changed = true
			}
		}
		c.mu.Unlock()
	}
	if changed {
		_ = c.sendCurrent(ctx, protocol.Frame{V: protocol.Version, Type: protocol.LauncherStatus, Launchers: c.launcherRows(), Capabilities: c.CapabilityReports})
	}
}

// probeLauncherFeatures asks a Docker launcher for its features (the probe exits before it touches Docker) and records them, or why they cannot be used, on the row. Doctor and Settings read them from there instead of probing again.
func probeLauncherFeatures(row *protocol.LauncherRow, def protocol.LauncherDefinition, path, checkout string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	features, err := adapter.DockerFeatures(ctx, path, def.Args, checkout, launcherEnvironment(def.EnvRefs, def.Tool))
	if err != nil {
		if !adapter.DockerNeedsForwardEnv(def.Tool) {
			return // the tool needs no feature: an answer that cannot be read only means none is known
		}
		row.FeatureProblem = "the launcher's feature probe failed: " + err.Error()
		return
	}
	for feature := range features {
		row.Features = append(row.Features, feature)
	}
	sort.Strings(row.Features)
	// Claude needs no forwarded variable: it asks for the features only to know whether its sessions are shared with the host.
	if adapter.DockerNeedsForwardEnv(def.Tool) && !features[adapter.FeatureForwardEnv] {
		row.FeatureProblem = "launcher too old: it does not forward environment variables, which " + def.Tool + " needs (update harness-docker)"
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

func (c *Client) setLauncher(def protocol.LauncherDefinition) error {
	c.mu.Lock()
	probeDir := c.harnessProbeDir
	c.mu.Unlock()
	row, err := inspectLauncher(def, probeDir)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Launchers {
		if c.Launchers[i].ID == def.ID {
			c.Launchers[i] = row
			return nil
		}
	}
	c.Launchers = append(c.Launchers, row)
	return nil
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
	return append([]protocol.LauncherRow(nil), c.Launchers...)
}
