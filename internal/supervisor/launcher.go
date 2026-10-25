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
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		args := append(append([]string{}, def.Args...), "--version")
		cmd := exec.CommandContext(ctx, path, args...)
		if checkout != "" {
			cmd.Dir = checkout
		}
		cmd.Env = launcherEnvironment(def.EnvRefs, def.Tool)
		if output, err := cmd.Output(); err == nil {
			row.Version = strings.TrimSpace(string(output))
		} else if !def.Containerized {
			row.UnavailableReason = "launcher version check failed"
		}
	}
	if def.Containerized && adapter.DockerNeedsForwardEnv(def.Tool) {
		probeLauncherFeatures(&row, def, path, checkout)
	}
	if !def.Enabled {
		row.UnavailableReason = "disabled by owner"
	} else if row.UnavailableReason == "" {
		row.Available = true
	}
	return row, nil
}

// probeLauncherFeatures asks a Docker launcher for its features (the probe exits before it touches Docker, so it needs no checkout) and
// records them, or why they cannot be used, on the row. Doctor and Settings read them from there instead of probing again.
func probeLauncherFeatures(row *protocol.LauncherRow, def protocol.LauncherDefinition, path, checkout string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	features, err := adapter.DockerFeatures(ctx, path, def.Args, checkout, launcherEnvironment(def.EnvRefs, def.Tool))
	if err != nil {
		row.FeatureProblem = "the launcher's feature probe failed: " + err.Error()
		return
	}
	for feature := range features {
		row.Features = append(row.Features, feature)
	}
	sort.Strings(row.Features)
	if !features[adapter.FeatureForwardEnv] {
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
