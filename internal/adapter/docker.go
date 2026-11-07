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

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/cmdprobe"
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
const AgentKeyEnv = "AIMEBU2_AGENT_KEY"

// DockerAgentKeyEnv is what a Docker turn adds to the launcher's environment so the harness can read the agent's key from AgentKeyEnv: the
// variable itself and the forwarding list that names it. They are set last, so an owner's environment reference cannot replace either.
func DockerAgentKeyEnv(authorization string) []string {
	return []string{AgentKeyEnv + "=" + strings.TrimPrefix(authorization, "Bearer "), "HARNESS_DOCKER_FORWARD_ENV=" + AgentKeyEnv}
}

// DockerFeatures asks a Docker launcher which features it has. With HARNESS_DOCKER_PRINT_FEATURES=1 a launcher that knows the probe prints
// them, one per line, and exits before it touches Docker. One that predates it runs the harness with the arguments instead, which is why
// the probe always carries --version: its answer is then a version string, which names no feature.
func DockerFeatures(ctx context.Context, command string, args []string, dir string, env []string) (map[string]bool, error) {
	probe := exec.CommandContext(ctx, command, append(append([]string(nil), args...), "--version")...)
	probe.Dir = dir
	probe.Env = append(append([]string(nil), env...), "HARNESS_DOCKER_PRINT_FEATURES=1")
	out, err := cmdprobe.CombinedOutput(probe)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("Docker launcher feature probe: %w", ctx.Err())
		}
		return nil, fmt.Errorf("Docker launcher feature probe: %s: %w", SanitizeStderr(string(out)), err)
	}
	features := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && featureName.MatchString(line) {
			features[line] = true
		}
	}
	return features, nil
}

var featureName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// RequireDockerFeature refuses a launcher that lacks a feature the tool depends on, with the way out in the message.
func RequireDockerFeature(ctx context.Context, tool, command string, args []string, dir string, env []string, feature string) error {
	features, err := DockerFeatures(ctx, command, args, dir, env)
	if err != nil {
		return err
	}
	if !features[feature] {
		return fmt.Errorf("the Docker launcher %s is too old for %s: it does not %s (update harness-docker, then retry)", command, tool, featureDescription(feature))
	}
	return nil
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", checkout, "rev-parse", "--path-format=absolute", "--git-common-dir")
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

// DockerMCP reads the supervisor-private credential on the host. Docker turns
// pass an HTTP MCP endpoint inline and the key in a forwarded variable, so no key-bearing file enters a checkout.
func DockerMCP(path, agentID string) (endpoint, authorization string, err error) {
	return DockerMCPAt(path, agentID, "")
}

func DockerMCPAt(path, agentID, visibleServerURL string) (endpoint, authorization string, err error) {
	endpoint, authorization, err = agentHTTPMCP(path, agentID, visibleServerURL == "")
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

// NativeMCP uses the same validated private credential without rewriting the
// server address for a container.
func NativeMCP(path, agentID string) (endpoint, authorization string, err error) {
	return agentHTTPMCP(path, agentID, false)
}

func agentHTTPMCP(path, agentID string, docker bool) (endpoint, authorization string, err error) {
	if path == "" {
		return "", "", errors.New("normal turn requires agent tools config")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var config struct {
		Servers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", "", err
	}
	env := config.Servers["aimebu"].Env
	if env["AIMEBU2_AGENT_ID"] != agentID || !strings.HasPrefix(env["AIMEBU2_AGENT_KEY"], "aak_") {
		return "", "", errors.New("invalid private aimebu MCP config")
	}
	u, err := url.Parse(env["AIMEBU2_URL"])
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
	return u.String(), "Bearer " + env["AIMEBU2_AGENT_KEY"], nil
}
