package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/agentlog"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/supervisor"
	"github.com/hrubymar10/aimebu/internal/tlsclient"
)

// `aimebu doctor --machine` is for a host that runs only the supervisor, with
// no server or database of its own. It reads what lives on this machine: the
// pairing credential, the supervisor's journal, its private agent-tools config
// and agent logs, the harness executables and Docker, each Docker launcher's
// feature probe, and what the paired server says about this machine.

const (
	// agentLogsWarnFraction is how full the agent-log store may get, as a
	// fraction of the sweep's cap, before doctor mentions it.
	agentLogsWarnFraction = 0.8

	machineProbeTimeout  = 10 * time.Second
	machinePairStatusMax = 4096
)

// Seams for tests: the harness search and the Docker launcher probe run real
// executables.
var (
	doctorDetectHarnesses = supervisor.DetectHarnesses
	doctorDockerFeatures  = adapter.DockerFeatures
)

// checkMachineMode runs the supervisor-host checks for the config root.
func checkMachineMode(ctx context.Context, config serverConfig) []doctorCheck {
	var checks []doctorCheck
	credential, credentialCheck := checkMachineCredential(config.root)
	checks = append(checks, credentialCheck)
	checks = append(checks, checkSupervisorJournal(filepath.Join(config.root, "supervisor", "journal.sqlite")))
	checks = append(checks, checkAgentToolsConfig(filepath.Join(config.root, "supervisor", "agent-tools")))
	checks = append(checks, checkAgentLogs(filepath.Join(config.root, "supervisor", "agent-logs")))
	candidates := doctorDetectHarnesses(ctx)
	checks = append(checks, checkHarnessExecutables(candidates))
	checks = append(checks, checkDockerForLaunchers(candidates)...)
	checks = append(checks, checkDockerLauncherFeatures(ctx, candidates)...)
	if credential != nil {
		serverURL := os.Getenv("AIMEBU2_URL")
		if serverURL == "" {
			serverURL = credential.ServerURL
		}
		if serverURL == "" {
			serverURL = "http://127.0.0.1:" + strconv.Itoa(config.port)
		}
		checks = append(checks, checkPairedServer(ctx, serverURL, *credential))
		checks = append(checks, checkProviderKeys(ctx, serverURL, *credential)...)
	}
	return scopeAll(checks, scopeMachine)
}

// checkMachineCredential reads the supervisor's pairing credential: its own
// supervisor/machine.key, or on the server's host server/host-machine.key.
// None is a warning, not a failure: a host that has not been paired yet is a
// normal state. Permissions are checked by the same rule the supervisor
// applies (mode 0600).
func checkMachineCredential(root string) (*hostMachineCredential, doctorCheck) {
	const name = "machine credential"
	paths := []string{filepath.Join(root, "supervisor", "machine.key"), filepath.Join(root, "server", "host-machine.key")}
	for _, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%s: %v", path, err)}
		}
		if info.Mode().Perm() != 0o600 {
			return nil, doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%s: mode %o, the supervisor refuses it unless it is 0600", path, info.Mode().Perm()),
				Hint: "chmod 600 " + path}
		}
		credential, err := readHostMachineKey(path)
		if err != nil {
			return nil, doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%s: %v", path, err),
				Hint: "Pair this machine again: remove the file and run aimebu supervisor --pair <server-url> <token> with a fresh token."}
		}
		where := credential.ServerURL
		if where == "" {
			where = "the local server"
		}
		return &credential, doctorCheck{Name: name, Status: statusOK, Description: fmt.Sprintf("%s: machine %s, server %s", path, credential.MachineID, where)}
	}
	return nil, doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("no credential at %s: this host is not paired", paths[0]),
		Hint: "Create a pairing token in Settings → Machines, then run aimebu supervisor --pair <server-url> <token>."}
}

// checkSupervisorJournal opens the journal read-only for a bounded
// quick_check and reports its size. A held lock on its .lock file means a
// supervisor is running on it right now; that is noted, not judged.
func checkSupervisorJournal(path string) doctorCheck {
	const name = "supervisor journal"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return doctorCheck{Name: name, Status: statusOK, Description: "none yet (created when the supervisor first starts)"}
	} else if err != nil {
		return doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%s: %v", path, err)}
	}
	detail := fmt.Sprintf("%s, wal %s", humanBytes(fileSize(path)), humanBytes(fileSize(path+"-wal")))
	if pid, running := journalHolder(path + ".lock"); running {
		detail += fmt.Sprintf("; in use by supervisor pid %s", pid)
	}
	result, status := quickCheckBackup(path)
	switch status {
	case statusFail:
		return doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%s: %s", detail, result),
			Hint: "Stop the supervisor. A damaged journal can lose track of running agents; restore it from a copy, or remove it only if no agent should be resumed."}
	case statusWarn:
		return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s: %s", detail, result)}
	}
	return doctorCheck{Name: name, Status: statusOK, Description: fmt.Sprintf("%s: %s", detail, result)}
}

// journalHolder reports whether another process holds the journal's flock, and
// the pid it wrote there. It only reads: the file is opened read-only and the
// lock is taken shared and released at once.
func journalHolder(lockPath string) (string, bool) {
	file, err := os.Open(lockPath)
	if err != nil {
		return "", false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			pid := "unknown"
			if data, err := io.ReadAll(io.LimitReader(file, 32)); err == nil && strings.TrimSpace(string(data)) != "" {
				pid = strings.TrimSpace(string(data))
			}
			return pid, true
		}
		return "", false
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return "", false
}

// checkAgentToolsConfig verifies the per-agent toolbox configs the supervisor
// writes hold an agent key: the folder must be private (0700) and each file
// 0600, so another user on the machine cannot read an agent's credential.
func checkAgentToolsConfig(dir string) doctorCheck {
	const name = "agent tools config"
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return doctorCheck{Name: name, Status: statusOK, Description: "none yet (written when an agent starts)"}
	}
	if err != nil {
		return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("read %s: %v", dir, err)}
	}
	var loose []string
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o077 != 0 {
		loose = append(loose, fmt.Sprintf("folder mode %o", info.Mode().Perm()))
	}
	files := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		files++
		if info, err := entry.Info(); err == nil && info.Mode().Perm()&0o077 != 0 {
			loose = append(loose, fmt.Sprintf("%s mode %o", entry.Name(), info.Mode().Perm()))
		}
	}
	if len(loose) > 0 {
		if len(loose) > 5 {
			loose = append(loose[:5], "…")
		}
		return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%d config(s); readable by others: %s", files, strings.Join(loose, ", ")),
			Hint: "chmod -R go-rwx " + dir + " (they hold agent keys)."}
	}
	return doctorCheck{Name: name, Status: statusOK, Description: fmt.Sprintf("%d config(s), private (0700/0600)", files)}
}

// checkAgentLogs reports the size of the captured agent output against the
// sweep's total cap. The supervisor sweeps daily, so only a store near its cap
// is worth a mention.
func checkAgentLogs(dir string) doctorCheck {
	const name = "agent logs"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return doctorCheck{Name: name, Status: statusOK, Description: "none yet"}
	}
	bytes, agents := agentlog.New(dir).Usage()
	detail := fmt.Sprintf("%s across %d agent(s) (cap %s)", humanBytes(uint64(bytes)), agents, humanBytes(agentlog.DefaultMaxTotal))
	if float64(bytes) >= agentLogsWarnFraction*float64(agentlog.DefaultMaxTotal) {
		return doctorCheck{Name: name, Status: statusWarn, Description: detail + ": near the cap, the oldest agents' logs are about to be swept",
			Hint: "Nothing to fix unless you need those logs: read them from the agent's page before the daily sweep removes them."}
	}
	return doctorCheck{Name: name, Status: statusOK, Description: detail}
}

// checkHarnessExecutables lists the native harness executables the supervisor
// can find, with their versions. None found warns: this machine could not run
// an agent natively (Docker launchers are judged separately).
func checkHarnessExecutables(candidates []protocol.HarnessCandidate) doctorCheck {
	const name = "harnesses"
	var native []string
	for _, candidate := range candidates {
		if candidate.Containerized {
			continue
		}
		entry := candidate.Family + " " + candidate.Command
		if candidate.Version != "" {
			entry += " (" + candidate.Version + ")"
		}
		native = append(native, entry)
	}
	if len(native) == 0 {
		return doctorCheck{Name: name, Status: statusWarn, Description: "no claude, codex, pi, opencode or vibe executable found on PATH or in the usual user install folders",
			Hint: "Install a harness on this machine, or set up a Docker launcher for it."}
	}
	sort.Strings(native)
	return doctorCheck{Name: name, Status: statusOK, Description: strings.Join(native, "; ")}
}

// checkDockerForLaunchers reports the Docker CLI and daemon. They only matter
// when a *-docker launcher is installed: then a missing daemon warns;
// otherwise the result is informational.
func checkDockerForLaunchers(candidates []protocol.HarnessCandidate) []doctorCheck {
	const name = "docker"
	hasLauncher := false
	for _, candidate := range candidates {
		hasLauncher = hasLauncher || candidate.Containerized
	}
	reachable := dockerReachable()
	switch {
	case reachable:
		return []doctorCheck{{Name: name, Status: statusOK, Description: "docker CLI found and the daemon answers"}}
	case hasLauncher:
		return []doctorCheck{{Name: name, Status: statusWarn, Description: "a *-docker launcher is installed, but docker is not on PATH or its daemon does not answer (`docker info` fails)",
			Hint: "Start Docker, or make docker available on the PATH of the supervisor."}}
	}
	return []doctorCheck{{Name: name, Status: statusOK, Description: "not available (no *-docker launcher is installed, so none is needed)"}}
}

// checkDockerLauncherFeatures runs the feature probe (HARNESS_DOCKER_PRINT_FEATURES=1 <launcher> --version) of each Docker launcher whose
// tool depends on a feature. A launcher that knows the probe exits before it touches Docker; one that predates it ignores the variable and
// starts a container running `<harness> --version`. That is why only the tools whose Docker turns need a feature are probed (the supervisor
// refuses an old launcher for them anyway), and why the probe runs from a fresh, empty temporary directory rather than the caller's: an old
// launcher then mounts nothing of the caller's. A launcher without the feature is "too old": the supervisor refuses it for that tool.
func checkDockerLauncherFeatures(ctx context.Context, candidates []protocol.HarnessCandidate) []doctorCheck {
	var checks []doctorCheck
	var dir string
	for _, candidate := range candidates {
		if !candidate.Containerized || !adapter.DockerNeedsForwardEnv(candidate.Family) {
			continue
		}
		name := "launcher " + filepath.Base(candidate.Command)
		if dir == "" {
			var err error
			if dir, err = os.MkdirTemp("", "aimebu-doctor-probe-"); err != nil {
				return append(checks, doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("cannot make an empty folder to probe in: %v", err)})
			}
			defer os.RemoveAll(dir)
		}
		probe, cancel := context.WithTimeout(ctx, machineProbeTimeout)
		features, err := doctorDockerFeatures(probe, candidate.Command, nil, dir, os.Environ())
		cancel()
		if err != nil {
			checks = append(checks, doctorCheck{Name: name, Status: statusWarn, Description: err.Error(),
				Hint: "Run " + candidate.Command + " --version yourself to see why the probe failed."})
			continue
		}
		var have []string
		for feature := range features {
			have = append(have, feature)
		}
		sort.Strings(have)
		list := strings.Join(have, ", ")
		if list == "" {
			list = "no features reported"
		}
		if !features[adapter.FeatureForwardEnv] {
			checks = append(checks, doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s: too old for %s (needs %s); turns on it are refused", list, candidate.Family, adapter.FeatureForwardEnv),
				Hint: "Update harness-docker, then run aimebu doctor --machine again."})
			continue
		}
		checks = append(checks, doctorCheck{Name: name, Status: statusOK, Description: list})
	}
	return checks
}

// checkPairedServer asks the server this machine is paired with whether it
// knows the machine and has approved it. The answer needs the machine's own
// key as a bearer token, the same request the supervisor makes at start.
func checkPairedServer(ctx context.Context, serverURL string, credential hostMachineCredential) doctorCheck {
	const name = "paired server"
	probe, cancel := context.WithTimeout(ctx, machineProbeTimeout)
	defer cancel()
	requestURL := strings.TrimRight(serverURL, "/") + "/api/machines/" + url.PathEscape(credential.MachineID) + "/pair-status"
	req, err := http.NewRequestWithContext(probe, http.MethodGet, requestURL, nil)
	if err != nil {
		return doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%s: %v", serverURL, err)}
	}
	req.Header.Set("Authorization", "Bearer "+credential.Key)
	resp, err := tlsclient.NewHTTPClient(machineProbeTimeout).Do(req)
	if err != nil {
		return doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%s unreachable: %v", serverURL, err),
			Hint: "Check the server is running and reachable from this machine, and that AIMEBU2_URL (or the paired URL) is right."}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var status struct {
			Approved bool   `json:"approved"`
			Name     string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, machinePairStatusMax)).Decode(&status); err != nil {
			return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s answered with something unreadable: %v", serverURL, err)}
		}
		if !status.Approved {
			return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s knows machine %s but has not approved it yet", serverURL, credential.MachineID),
				Hint: "Approve the machine in Settings → Machines on the server."}
		}
		return doctorCheck{Name: name, Status: statusOK, Description: fmt.Sprintf("%s: machine %s is approved", serverURL, credential.MachineID)}
	case http.StatusUnauthorized:
		return doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%s rejected this machine's key (revoked, or paired with another server)", serverURL),
			Hint: "Pair this machine again with a fresh token (remove supervisor/machine.key first)."}
	}
	return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s answered HTTP %d to the approval check", serverURL, resp.StatusCode)}
}

// checkProviderKeys asks the paired server (with the machine's key, as the approval check does) which key variables the custom providers of
// this machine's OpenCode harnesses are read from, and warns about those with no value in this environment, where a turn on that provider
// would be refused up front. It says nothing when the server cannot be reached (the paired server check already does) or no provider names
// a key. Only names cross the wire, and only whether each is set is reported.
func checkProviderKeys(ctx context.Context, serverURL string, credential hostMachineCredential) []doctorCheck {
	const name = "custom provider keys"
	probe, cancel := context.WithTimeout(ctx, machineProbeTimeout)
	defer cancel()
	requestURL := strings.TrimRight(serverURL, "/") + "/api/machines/" + url.PathEscape(credential.MachineID) + "/pair-status"
	req, err := http.NewRequestWithContext(probe, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+credential.Key)
	resp, err := tlsclient.NewHTTPClient(machineProbeTimeout).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var status struct {
		Approved     bool `json:"approved"`
		ProviderKeys []struct {
			Launcher   string            `json:"launcher"`
			ProviderID string            `json:"provider_id"`
			KeyEnv     string            `json:"key_env"`
			EnvRefs    map[string]string `json:"env_refs"`
		} `json:"provider_keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, machinePairStatusMax*16)).Decode(&status); err != nil || !status.Approved || len(status.ProviderKeys) == 0 {
		return nil
	}
	var missing []string
	for _, key := range status.ProviderKeys {
		if adapter.ProtectedKeyEnv(key.KeyEnv) || adapter.EnvValue(key.KeyEnv, key.EnvRefs) == "" {
			missing = append(missing, fmt.Sprintf("%s (provider %s of %s)", key.KeyEnv, key.ProviderID, key.Launcher))
		}
	}
	if len(missing) == 0 {
		return []doctorCheck{{Name: name, Status: statusOK, Description: fmt.Sprintf("%d key variable(s) of custom OpenCode providers are set here", len(status.ProviderKeys))}}
	}
	sort.Strings(missing)
	return []doctorCheck{{Name: name, Status: statusWarn, Description: "not set here: " + strings.Join(missing, ", "),
		Hint: "A turn on that provider is refused up front until its key variable is set where the supervisor runs. Doctor sees this shell's environment; a supervisor started by a service manager has its own, so set it there (or in the harness's environment references)."}}
}
