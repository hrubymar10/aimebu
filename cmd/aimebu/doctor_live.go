package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/server"
)

const doctorLiveMaxBody = 1 << 20

// `aimebu doctor --live` asks the running server for what only it knows: the
// capability flags each connected supervisor announced and what its launchers
// last reported. It authenticates with the key the server wrote to
// server/doctor.key (mode 0600, accepted only from this host), so being able to read that file
// is the credential.

// readDoctorKey reads the key file, refusing one that is not private to this
// user: a key anyone can read authenticates anyone.
func readDoctorKey(root string) (string, doctorCheck, bool) {
	const name = "doctor key"
	path := filepath.Join(root, "server", server.DoctorKeyFile)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s does not exist: live checks need a running server that writes it", path),
			Hint: "Start (or restart) the server from this build; it writes the key at start and removes it at shutdown."}, false
	}
	if err != nil {
		return "", doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s: %v", path, err)}, false
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s is not a private regular file (mode %v): not used", path, info.Mode()),
			Hint: "Restart the server to write a fresh key, and keep the config directory private."}, false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return "", doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s belongs to another user: not used", path),
			Hint: "Run doctor as the user that runs the server."}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("%s: %v", path, err)}, false
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", doctorCheck{Name: name, Status: statusWarn, Description: path + " is empty", Hint: "Restart the server to write a fresh key."}, false
	}
	return key, doctorCheck{Name: name, Status: statusOK, Description: path + " is private (0600)"}, true
}

// doctorLiveChecks runs the --live checks against the server at bind:port.
func doctorLiveChecks(ctx context.Context, config serverConfig) []doctorCheck {
	key, keyCheck, ok := readDoctorKey(config.root)
	if !ok {
		return scopeAll([]doctorCheck{keyCheck}, scopeLive)
	}
	checks := []doctorCheck{keyCheck}
	live, failure := fetchDoctorLive(ctx, config, key)
	if failure != nil {
		return scopeAll(append(checks, *failure), scopeLive)
	}
	checks = append(checks, liveMachineChecks(live)...)
	return scopeAll(checks, scopeLive)
}

func fetchDoctorLive(ctx context.Context, config serverConfig, key string) (protocol.DoctorLive, *doctorCheck) {
	const name = "live server"
	requestURL := "http://" + net.JoinHostPort(config.bind, strconv.Itoa(config.port)) + "/api/doctor"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return protocol.DoctorLive{}, &doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("GET %s: %v", requestURL, err)}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := doctorHTTPClient(5 * time.Second).Do(req)
	if err != nil {
		return protocol.DoctorLive{}, &doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("GET %s: %v", requestURL, err), Hint: "Live checks need the server to answer on its HTTP port."}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return protocol.DoctorLive{}, &doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("the server at %s refused the doctor key", requestURL),
			Hint: "The key file belongs to another start of the server (it is replaced at every start; is another server on this port, or this config directory a copy?), or the request did not reach the server from this host directly."}
	case http.StatusNotFound:
		return protocol.DoctorLive{}, &doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("the server at %s has no /api/doctor", requestURL),
			Hint: "The server on this port is an older build. Restart it from this build."}
	default:
		return protocol.DoctorLive{}, &doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("GET %s: HTTP %d", requestURL, resp.StatusCode)}
	}
	var live protocol.DoctorLive
	if err := json.NewDecoder(io.LimitReader(resp.Body, doctorLiveMaxBody)).Decode(&live); err != nil {
		return protocol.DoctorLive{}, &doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("the answer of %s is unreadable: %v", requestURL, err)}
	}
	return live, nil
}

// liveMachineChecks turns the server's account of its machines into findings:
// per connected machine the capability flags its supervisor did not announce
// (with what degrades), and per enabled launcher what it reported.
func liveMachineChecks(live protocol.DoctorLive) []doctorCheck {
	var checks []doctorCheck
	connected := 0
	for _, machine := range live.Machines {
		if !machine.Approved {
			continue
		}
		label := machine.Name
		if label == "" {
			label = machine.ID
		}
		name := "machine " + label
		if !machine.Connected {
			checks = append(checks, doctorCheck{Name: name, Status: statusOK, Description: "not connected right now"})
			continue
		}
		connected++
		var missing []string
		for _, support := range machine.Supports {
			if !support.Supported {
				missing = append(missing, fmt.Sprintf("%s (%s)", support.Flag, support.Degrades))
			}
		}
		if len(missing) > 0 {
			checks = append(checks, doctorCheck{Name: name, Status: statusWarn, Description: "connected, but its supervisor does not announce: " + strings.Join(missing, "; "),
				Hint: "Update the supervisor on that machine to the same build as the server."})
		} else {
			checks = append(checks, doctorCheck{Name: name, Status: statusOK, Description: fmt.Sprintf("connected; all %d capabilities announced", len(machine.Supports))})
		}
		for _, launcher := range machine.Launchers {
			if !launcher.Enabled {
				continue
			}
			checks = append(checks, liveLauncherCheck(label, launcher))
		}
	}
	if connected == 0 {
		checks = append(checks, doctorCheck{Name: "connected machines", Status: statusWarn, Description: "no approved machine is connected: no agent can run",
			Hint: "Start a supervisor (the server starts its embedded one unless AIMEBU2_EMBEDDED_SUPERVISOR=0), or pair a remote machine."})
	}
	return checks
}

func liveLauncherCheck(machine string, launcher protocol.DoctorLauncher) doctorCheck {
	display := launcher.Label
	if display == "" {
		display = launcher.Tool + "/" + launcher.Name
	}
	name := "launcher " + display + " on " + machine
	switch {
	case !launcher.Reported:
		return doctorCheck{Name: name, Status: statusWarn, Description: "the supervisor has not reported this launcher yet",
			Hint: "Wait a moment and run doctor again; if it stays, restart the supervisor on that machine."}
	case !launcher.Available:
		reason := launcher.UnavailableReason
		if reason == "" {
			reason = "no reason given"
		}
		return doctorCheck{Name: name, Status: statusWarn, Description: "unavailable: " + reason, Hint: "Fix the launcher in Settings → Harnesses."}
	case launcher.Tool == "codex" && strings.Contains(launcher.FeatureProblem, "codex_home"):
		return doctorCheck{Name: name, Status: statusWarn, Description: launcher.FeatureProblem,
			Hint: "Start the container, or update harness-docker on that machine; then re-check in Settings → Harnesses."}
	case launcher.FeatureProblem != "":
		return doctorCheck{Name: name, Status: statusWarn, Description: launcher.FeatureProblem, Hint: "Update harness-docker on that machine, then re-check in Settings → Harnesses."}
	}
	description := "available"
	if launcher.Version != "" {
		description += ", " + launcher.Version
	}
	if len(launcher.Features) > 0 {
		description += "; features: " + strings.Join(launcher.Features, ", ")
	}
	return doctorCheck{Name: name, Status: statusOK, Description: description}
}
