package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

var _ adapter.WarmAdapter = (*Adapter)(nil)

type warmLaunch struct {
	command, dir, runtimeDir, visibleCheckout, model string
	args, env                                        []string
	config                                           []byte
	key                                              string
	authPath                                         string
	containerized                                    bool
}

// prepareWarm uses the same private Codex home and bridge configuration as
// exec. Session IDs and turn input are deliberately absent from the launch.
func (a *Adapter) prepareWarm(spec adapter.TurnSpec) (warmLaunch, error) {
	if spec.Mode != adapter.ModeNormal || spec.ForkSession {
		return warmLaunch{}, adapter.ErrNoWarm
	}
	if a.variant() == "native" && !a.AllowNative {
		return warmLaunch{}, errors.New("native execution disabled; use the docker variant")
	}
	if spec.TurnID == "" || spec.Agent.ID == "" || spec.Agent.Model == "" || spec.CheckoutPath == "" {
		return warmLaunch{}, errors.New("turn ID, agent ID, checkout path, and model required")
	}
	if spec.NativeSessionStarted && spec.NativeSessionHandle == "" {
		return warmLaunch{}, errors.New("resumed turn requires native session handle")
	}
	if a.variant() == "native" && a.RuntimeRoot == "" {
		return warmLaunch{}, errors.New("Codex runtime root required")
	}
	if a.variant() == "docker" {
		if err := adapter.DockerCheckout(spec.CheckoutPath); err != nil {
			return warmLaunch{}, err
		}
	}
	authPath := a.AuthPath
	if a.variant() == "docker" {
		authPath = a.WarmAuthPath(spec)
		if authPath == "" {
			return warmLaunch{}, fmt.Errorf("%w: Docker Codex home was not reported by the launcher", adapter.ErrNoWarm)
		}
	} else if authPath == "" {
		authPath = hostCodexAuthPath(spec)
	}
	if err := warmAuthAllowed(authPath, spec); err != nil {
		return warmLaunch{}, err
	}
	var own userMCP
	if a.variant() == "native" {
		own = readUserMCP(a.AuthPath)
	}
	runtimeDir, err := a.prepareRuntime(spec, own)
	if err != nil {
		return warmLaunch{}, err
	}
	visible, err := adapter.VisiblePath(spec, spec.CheckoutPath)
	if err != nil {
		return warmLaunch{}, err
	}
	l := warmLaunch{dir: spec.CheckoutPath, runtimeDir: runtimeDir, visibleCheckout: visible,
		model: spec.Agent.Model, containerized: a.variant() == "docker" || spec.LauncherContainerized}
	l.command = a.Command
	if spec.LauncherPath != "" {
		l.command = spec.LauncherPath
	}
	if a.variant() == "docker" {
		l.authPath = authPath
		config, overrides, err := warmDockerConfig(spec)
		if err != nil {
			return warmLaunch{}, err
		}
		l.config = config
		endpoint, header, err := adapter.DockerMCPAt(spec.AgentTools, spec.Agent.ID, spec.AgentServerURL)
		if err != nil {
			return warmLaunch{}, err
		}
		l.key = strings.Join(adapter.DockerAgentKeyEnv(header), "")
		l.env = append(childEnv(""), "CODEX_LAUNCH_FLAGS=")
		for _, override := range overrides {
			l.args = append(l.args, "-c", override)
		}
		l.args = append(l.args, "-c", `mcp_servers.aimebu={command="true",enabled=false}`, "-c",
			"mcp_servers.aimebu_docker={url="+strconv.Quote(endpoint)+",bearer_token_env_var="+strconv.Quote(adapter.AgentKeyEnv)+"}")
	} else {
		l.authPath = a.AuthPath
		l.env = childEnv(runtimeDir, own.envVars...)
		data, err := os.ReadFile(filepath.Join(runtimeDir, "config.toml"))
		if err != nil {
			return warmLaunch{}, err
		}
		l.config = data
		if spec.AgentTools != nil {
			l.key = adapter.AgentKeyEnv + "=" + spec.AgentTools.Key
		}
	}
	l.args = append(l.args, "app-server")
	l.args = adapter.LauncherArgs(spec, l.args)
	l.env = adapter.LauncherEnv(l.env, spec.LauncherEnvRefs)
	if l.key != "" {
		l.env = append(l.env, l.key)
	}
	return l, nil
}

func (l warmLaunch) fingerprint() string {
	h := sha256.New()
	add := func(value string) { fmt.Fprintf(h, "%d:%s\n", len(value), value) }
	add(l.command)
	add(l.dir)
	add(l.visibleCheckout)
	add(l.model)
	add(l.authPath)
	for _, arg := range l.args {
		add(arg)
	}
	env := append([]string(nil), l.env...)
	sort.Strings(env)
	for _, item := range env {
		if item == l.key && l.key != "" {
			sum := sha256.Sum256([]byte(item))
			item = strings.SplitN(item, "=", 2)[0] + "=sha256:" + hex.EncodeToString(sum[:8])
		}
		add(item)
	}
	h.Write(l.config)
	return hex.EncodeToString(h.Sum(nil))
}

func (a *Adapter) WarmFingerprint(spec adapter.TurnSpec) (string, error) {
	l, err := a.prepareWarm(spec)
	if err != nil {
		return "", err
	}
	return l.fingerprint(), nil
}

// StartWarm launches JSON-RPC over stdio. A failed handshake leaves no live
// process behind and lets the supervisor run the same turn through exec.
func (a *Adapter) StartWarm(ctx context.Context, spec adapter.TurnSpec) (adapter.WarmSession, error) {
	l, err := a.prepareWarm(spec)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, l.command, l.args...)
	cmd.Dir, cmd.Env = l.dir, l.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var container *adapter.ContainerIdentity
	if l.containerized && spec.LauncherReportsContainer {
		// Asked of the process only, not of the fingerprint: what the launcher prints about itself changes nothing the harness is started with.
		container = &adapter.ContainerIdentity{}
		cmd.Env = append(cmd.Env[:len(cmd.Env):len(cmd.Env)], adapter.DockerReportExecIDEnv+"=1")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := newWarmSession(cmd, stdin, l.fingerprint(), l.runtimeDir, l.visibleCheckout, l.authPath,
		l.containerized, adapter.JoinWriters(spec.StderrSink, a.stderrCapture), a.stdoutCapture)
	w.container = container
	go w.read(stdout, stderr)
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var answer struct {
		CodexHome string `json:"codexHome"`
	}
	err = w.call(initCtx, "initialize", map[string]any{"clientInfo": map[string]string{
		"name": "aimebu", "title": "aimebu", "version": "2"},
		"capabilities": map[string]bool{"experimentalApi": false, "requestAttestation": false}}, &answer)
	expectedHome := l.runtimeDir
	if a.variant() == "docker" {
		expectedHome = filepath.Dir(l.authPath)
	}
	if err == nil && filepath.Clean(answer.CodexHome) != filepath.Clean(expectedHome) {
		err = fmt.Errorf("Codex app-server home %q differs from expected home %q", answer.CodexHome, expectedHome)
	}
	if err == nil {
		err = w.notify("initialized", map[string]any{})
	}
	if err == nil {
		w.defaultEffort, err = w.modelDefaultEffort(initCtx, spec.Agent.Model)
	}
	if err != nil {
		_ = w.Close(2 * time.Second)
		return nil, err
	}
	return w, nil
}
