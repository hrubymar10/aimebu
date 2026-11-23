package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/cmdprobe"
	"github.com/hrubymar10/aimebu/v2/internal/runtimeignore"
	"github.com/hrubymar10/aimebu/v2/internal/worktree"
)

// DockerPreflight checks the requested checkout through the actual launcher.
// A bad mount fails the spawn instead of creating an uncertain first turn.
func DockerPreflight(tool, checkout string, launcherPath ...string) error {
	if tool != "claude" && tool != "codex" && tool != "pi" && tool != "opencode" && tool != "vibe" {
		return fmt.Errorf("unsupported Docker tool %q", tool)
	}
	if err := DockerCheckout(checkout); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	launcher := tool + "-docker"
	if len(launcherPath) > 0 && launcherPath[0] != "" {
		launcher = launcherPath[0]
	}
	if DockerNeedsForwardEnv(tool) {
		if err := RequireDockerFeature(ctx, tool, launcher, nil, checkout, os.Environ(), FeatureForwardEnv); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, launcher, "--version")
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(), strings.ToUpper(tool)+"_LAUNCH_FLAGS=")
	out, err := cmdprobe.CombinedOutput(cmd) // a launcher that exits 0 and leaves a helper behind passed its preflight
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Docker launcher preflight: %w", ctx.Err())
		}
		return fmt.Errorf("Docker launcher preflight: %s: %w", SanitizeStderr(string(out)), err)
	}
	return nil
}

// DockerBaseEnv is what a Docker launcher itself needs on the host: its search path, the user's home, the proxy and certificate settings, and
// the Docker client's own variables. Nothing of it reaches the harness: what the harness is to get is listed in HARNESS_DOCKER_FORWARD_ENV.
func DockerBaseEnv() []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"TERM": true, "TMPDIR": true, "HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
		"DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_TLS_VERIFY": true, "DOCKER_CERT_PATH": true}
	out := []string{}
	for _, item := range os.Environ() {
		if key, _, ok := strings.Cut(item, "="); ok && allowed[key] {
			out = append(out, item)
		}
	}
	return out
}

// FeatureForwardEnv is the Docker launcher feature that hands the variables named in HARNESS_DOCKER_FORWARD_ENV to the harness.
const FeatureForwardEnv = "forward_env"

// FeatureClaudeHomeShared is the Docker launcher feature that says the container works in the host's Claude configuration directory, mounted
// at the same path, with the working folders mirrored as well: a Claude Code session made on either side is found, under the same folder name,
// from the other. Only the Claude launcher reports it, and a launcher that predates it does not: that means "unknown", never "shared".
const FeatureClaudeHomeShared = "claude_home_shared"

// DockerNeedsForwardEnv says whether a tool's Docker turns configure the harness through forwarded environment variables, which an older
// launcher would silently drop: the turn would then run without its isolated configuration or its toolbox.
func DockerNeedsForwardEnv(tool string) bool {
	return tool == "opencode" || tool == "vibe" || tool == "claude" || tool == "codex"
}

// AgentKeyEnv is the variable that carries an agent's key into a Docker harness. The key never appears in an argument list (ps shows those
// to every local user): the harness's MCP entry names this variable, and the launcher hands it over by name (HARNESS_DOCKER_FORWARD_ENV).
const AgentKeyEnv = "AIMEBU_AGENT_KEY"

// DockerAgentKeyEnv is what a Docker turn adds to the launcher's environment so the harness can read the agent's key from AgentKeyEnv: the
// variable itself and the forwarding list that names it. They are set last, so an owner's environment reference cannot replace either.
func DockerAgentKeyEnv(authorization string) []string {
	return []string{AgentKeyEnv + "=" + strings.TrimPrefix(authorization, "Bearer "), "HARNESS_DOCKER_FORWARD_ENV=" + AgentKeyEnv}
}

// DockerFeatureReport is the launcher's one cached probe answer. CodexHome is
// present only when the launcher can read the running container's CODEX_HOME.
type DockerFeatureReport struct {
	Features  map[string]bool
	CodexHome string
}

// DockerProbe asks a Docker launcher for features and, for Codex, the actual
// container home. Older launchers answer --version with no recognized lines.
func DockerProbe(ctx context.Context, command string, args []string, dir string, env []string) (DockerFeatureReport, error) {
	probe := exec.CommandContext(ctx, command, append(append([]string(nil), args...), "--version")...)
	probe.Dir = dir
	probe.Env = append(append([]string(nil), env...), "HARNESS_DOCKER_PRINT_FEATURES=1")
	out, err := cmdprobe.CombinedOutput(probe)
	if err != nil {
		if ctx.Err() != nil {
			return DockerFeatureReport{}, fmt.Errorf("Docker launcher feature probe: %w", ctx.Err())
		}
		return DockerFeatureReport{}, fmt.Errorf("Docker launcher feature probe: %s: %w", SanitizeStderr(string(out)), err)
	}
	report := DockerFeatureReport{Features: map[string]bool{}}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if home, ok := strings.CutPrefix(line, "codex_home="); ok && filepath.IsAbs(home) {
			report.CodexHome = filepath.Clean(home)
		} else if line != "" && featureName.MatchString(line) {
			report.Features[line] = true
		}
	}
	return report, nil
}

// DockerFeatures is the feature-only view used by ordinary preflight.
func DockerFeatures(ctx context.Context, command string, args []string, dir string, env []string) (map[string]bool, error) {
	report, err := DockerProbe(ctx, command, args, dir, env)
	return report.Features, err
}

var featureName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// RequireDockerFeature refuses a launcher that lacks a feature the tool depends on, with the way out in the message.
func RequireDockerFeature(ctx context.Context, tool, command string, args []string, dir string, env []string, feature string) error {
	features, err := DockerFeatures(ctx, command, args, dir, env)
	if err != nil {
		return err
	}
	return CheckDockerFeature(features, tool, command, feature)
}

func featureDescription(feature string) string {
	if feature == FeatureForwardEnv {
		return "forward environment variables to the harness (HARNESS_DOCKER_FORWARD_ENV)"
	}
	return "support " + feature
}

// DockerCheckout keeps generated turn files below the mirrored checkout.
// The launcher itself rejects a cwd that is not mounted in its container.
func DockerCheckout(checkout string) error {
	if !filepath.IsAbs(checkout) {
		return errors.New("Docker checkout path must be absolute")
	}
	info, err := os.Stat(checkout)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("Docker checkout path is not a directory")
	}
	path := filepath.Join(checkout, ".aimebu")
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory", path)
	}
	return ExcludeRuntimeState(checkout)
}

// ExcludeRuntimeState keeps an agent's runtime state (.aimebu/: session transcripts, generated configs) out of the repository's index, so an
// agent's `git add -A && git push` cannot publish it. The exclude file is the repository's own (info/exclude, shared by its worktrees), which
// no commit carries. A folder that is not a git checkout has nothing to exclude it from and is left alone.
func ExcludeRuntimeState(checkout string) error {
	if err := runtimeignore.Ensure(checkout); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := worktree.GitCommand(ctx, checkout, "rev-parse", "--path-format=absolute", "--git-common-dir")
	out, err := cmdprobe.Output(cmd)
	if err != nil {
		return nil // not a git checkout (or git is not here): nothing to exclude it from
	}
	// The repository's shared directory (the same for a linked worktree), and the exclude file named under it here rather than by git, whose own
	// answer for the file would already be the target of a link.
	common := strings.TrimSpace(string(out))
	if common == "" || !filepath.IsAbs(common) {
		return nil
	}
	exclude := filepath.Join(common, "info", "exclude")
	// The repository's own file, in a folder an agent can write to when it is mounted into its container: a link there would make this append the
	// line to whatever the link names, so a link is refused and the file is opened without following one.
	if info, err := os.Lstat(exclude); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a link; refusing to write the exclude line through it", exclude)
	}
	data, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == ".aimebu/" {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(exclude, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	line := ".aimebu/\n"
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		line = "\n" + line
	}
	_, err = file.WriteString(line)
	return err
}

// DockerMCP turns the turn's own agent access into an HTTP MCP endpoint and bearer. Docker turns pass the endpoint inline and the key in a
// forwarded variable, so no key-bearing file enters a checkout.
func DockerMCP(access *AgentToolsAccess, agentID string) (endpoint, authorization string, err error) {
	return DockerMCPAt(access, agentID, "")
}

func DockerMCPAt(access *AgentToolsAccess, agentID, visibleServerURL string) (endpoint, authorization string, err error) {
	endpoint, authorization, err = agentHTTPMCP(access, agentID, visibleServerURL == "")
	if err != nil || visibleServerURL == "" {
		return endpoint, authorization, err
	}
	visible, err := url.Parse(visibleServerURL)
	if err != nil || (visible.Scheme != "http" && visible.Scheme != "https") || visible.Hostname() == "" {
		return "", "", errors.New("invalid launcher agent server URL")
	}
	visible.Path = "/api/agent/mcp"
	visible.RawQuery, visible.Fragment = "", ""
	return visible.String(), authorization, nil
}

// NativeMCP validates the same access without rewriting the server address for a container.
func NativeMCP(access *AgentToolsAccess, agentID string) (endpoint, authorization string, err error) {
	return agentHTTPMCP(access, agentID, false)
}

func agentHTTPMCP(access *AgentToolsAccess, agentID string, docker bool) (endpoint, authorization string, err error) {
	if access == nil {
		return "", "", errors.New("normal turn requires agent tools access")
	}
	env := map[string]string{"AIMEBU_URL": access.ServerURL, "AIMEBU_AGENT_ID": access.AgentID, AgentKeyEnv: access.Key}
	if env["AIMEBU_AGENT_ID"] != agentID || !strings.HasPrefix(env[AgentKeyEnv], "aak_") {
		return "", "", errors.New("invalid agent tools access")
	}
	u, err := url.Parse(env["AIMEBU_URL"])
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" {
		return "", "", errors.New("invalid aimebu server URL")
	}
	if docker && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		port := u.Port()
		u.Host = "host.docker.internal"
		if port != "" {
			u.Host += ":" + port
		}
	}
	u.Path = "/api/agent/mcp"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), "Bearer " + env[AgentKeyEnv], nil
}

// BridgeEntry is the stdio MCP entry of `aimebu agent-tools` for a native turn: the server address, the agent's id and the rest of the bridge's
// environment, but never the key. The key is NamedKey in the entry's environment, which the harness expands (or forwards) from its own, where
// the turn put it, so it is in neither the entry nor any file.
func BridgeEntry(access *AgentToolsAccess, agentID, namedKey string) (command string, args []string, env map[string]string, err error) {
	if access == nil {
		return "", nil, nil, errors.New("normal turn requires agent tools access")
	}
	if _, _, err := agentHTTPMCP(access, agentID, false); err != nil {
		return "", nil, nil, err
	}
	if !filepath.IsAbs(access.Command) {
		return "", nil, nil, errors.New("invalid agent tools access")
	}
	env = map[string]string{"AIMEBU_URL": access.ServerURL, "AIMEBU_AGENT_ID": access.AgentID}
	for name, value := range access.Env {
		env[name] = value
	}
	if namedKey != "" {
		env[AgentKeyEnv] = namedKey
	}
	return access.Command, []string{"agent-tools"}, env, nil
}
