package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/client"
)

type fleetAgentConfig struct {
	Command      string `json:"command"`
	WrapTerminal *bool  `json:"wrap_terminal,omitempty"`
	AutoSetCwd   *bool  `json:"auto_set_cwd,omitempty"`
}

type fleetConfig struct {
	Agents []fleetAgentConfig `json:"agents"`
}

type fleetsResponse struct {
	Version int                    `json:"version"`
	Fleets  map[string]fleetConfig `json:"fleets"`
	Error   string                 `json:"error"`
}

type fleetResponse struct {
	Name  string      `json:"name"`
	Fleet fleetConfig `json:"fleet"`
	Error string      `json:"error"`
}

func fleetCmd(args []string) {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printFleetUsage()
		return
	}
	if err := fleetPlatformCheck(runtime.GOOS); err != nil {
		fatal("fleet", err)
	}
	c := client.DefaultClient()
	if len(args) == 0 {
		listFleets(c)
		return
	}
	if len(args) > 2 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu fleet <name> [path]")
		os.Exit(1)
	}
	targetPath, err := resolveFleetPath(args[1:])
	if err != nil {
		fatal("fleet", err)
	}
	resp, err := loadFleet(c, args[0])
	if err != nil {
		fatal("fleet", err)
	}
	if err := validateFleetLaunchConfig(resp.Fleet); err != nil {
		fatal("fleet", err)
	}
	failures := launchFleet(args[0], targetPath, resp.Fleet)
	if failures > 0 {
		os.Exit(failures)
	}
}

func fleetPlatformCheck(goos string) error {
	if goos != "darwin" {
		return errors.New("aimebu fleet currently only supports macOS (uses osascript + Terminal.app)")
	}
	return nil
}

func printFleetUsage() {
	fmt.Println(`Usage: aimebu fleet [<name> [path]]

List configured fleets, or launch a named fleet against path. When path is
omitted, the current working directory is used. The path is resolved to an
absolute path before command substitution.

Placeholders replaced before running each command:
  ${AIMEBU_FLEET_PATH}          absolute target path
  ${AIMEBU_FLEET_NAME}          fleet name
  ${AIMEBU_FLEET_AGENT_INDEX}   zero-based agent index`)
}

func listFleets(c *client.Client) {
	raw, err := c.Get("/fleets")
	if err != nil {
		fatal("fleet", err)
	}
	var resp fleetsResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		fatal("fleet", fmt.Errorf("parse /fleets response: %w", err))
	}
	if resp.Error != "" {
		fatal("fleet", errors.New(resp.Error))
	}
	if len(resp.Fleets) == 0 {
		fmt.Println("No fleets configured.")
		return
	}
	names := make([]string, 0, len(resp.Fleets))
	for name := range resp.Fleets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s\t%d agent(s)\n", name, len(resp.Fleets[name].Agents))
	}
}

func loadFleet(c *client.Client, name string) (fleetResponse, error) {
	raw, err := c.Get("/fleets/" + url.PathEscape(name))
	if err != nil {
		return fleetResponse{}, err
	}
	var resp fleetResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return fleetResponse{}, fmt.Errorf("parse fleet response: %w", err)
	}
	if resp.Error != "" {
		return fleetResponse{}, errors.New(resp.Error)
	}
	return resp, nil
}

func resolveFleetPath(args []string) (string, error) {
	var p string
	if len(args) == 0 {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd
	} else {
		p = args[0]
	}
	expanded, err := expandHomePath(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}

func expandHomePath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

func substituteFleetCommand(command, name, path string, index int) string {
	out := strings.ReplaceAll(command, "${AIMEBU_FLEET_PATH}", path)
	out = strings.ReplaceAll(out, "${AIMEBU_FLEET_NAME}", name)
	out = strings.ReplaceAll(out, "${AIMEBU_FLEET_AGENT_INDEX}", fmt.Sprintf("%d", index))
	return out
}

func fleetAgentBool(v *bool) bool {
	return v == nil || *v
}

func validateFleetLaunchConfig(fleet fleetConfig) error {
	for i, agent := range fleet.Agents {
		if !fleetAgentBool(agent.WrapTerminal) {
			return fmt.Errorf("agent %d: wrap_terminal must be true (v1)", i)
		}
	}
	return nil
}

func planFleetCommand(agent fleetAgentConfig, name, path string, index int) string {
	command := agent.Command
	if fleetAgentBool(agent.AutoSetCwd) {
		command = "cd ${AIMEBU_FLEET_PATH} && " + command
	}
	command = substituteFleetCommand(command, name, path, index)
	if fleetAgentBool(agent.WrapTerminal) {
		command = `osascript -e 'tell application "Terminal" to do script "` + escapeAppleScriptCommand(command) + `"'`
	}
	return command
}

func escapeAppleScriptCommand(command string) string {
	out := strings.ReplaceAll(command, `\`, `\\`)
	out = strings.ReplaceAll(out, `"`, `\"`)
	return out
}

func launchFleet(name, targetPath string, fleet fleetConfig) int {
	return launchFleetWithStarter(name, targetPath, fleet, func(command string) error {
		cmd := shellCommand(command)
		if err := cmd.Start(); err != nil {
			return err
		}
		if err := cmd.Process.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: detach failed for fleet command (pid %d): %v\n", cmd.Process.Pid, err)
		}
		return nil
	})
}

func launchFleetWithStarter(name, targetPath string, fleet fleetConfig, start func(command string) error) int {
	type launchFailure struct {
		index   int
		command string
		err     error
	}
	var failed []launchFailure
	for i, agent := range fleet.Agents {
		command := planFleetCommand(agent, name, targetPath, i)
		if err := start(command); err != nil {
			failed = append(failed, launchFailure{index: i, command: command, err: err})
		}
	}
	for _, failure := range failed {
		fmt.Fprintf(os.Stderr, "agent %d: %s\n  %v\n", failure.index, failure.command, failure.err)
	}
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "%d fleet command(s) failed to start.\n", len(failed))
	}
	return len(failed)
}

func shellCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd.exe", "/c", command)
	}
	return exec.Command("sh", "-c", command)
}
