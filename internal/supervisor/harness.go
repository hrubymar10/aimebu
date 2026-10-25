package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/cmdprobe"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

var harnessNames = []struct{ tool, variant, command string }{
	{"claude", "native", "claude"}, {"claude", "docker", "claude-docker"},
	{"codex", "native", "codex"}, {"codex", "docker", "codex-docker"},
	{"pi", "native", "pi"}, {"pi", "docker", "pi-docker"},
	{"opencode", "native", "opencode"},
	{"vibe", "native", "vibe"},
}

func harnessKey(tool, variant string) string { return tool + "\x00" + variant }

func knownHarness(tool, variant string) (string, bool) {
	for _, item := range harnessNames {
		if item.tool == tool && item.variant == variant {
			return item.command, true
		}
	}
	return "", false
}

func executablePath(path string) error {
	if !filepath.IsAbs(path) || strings.TrimSpace(path) != path || strings.ContainsAny(path, "\x00\n\r") {
		return errors.New("path must be absolute and contain no arguments")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("path must name a regular executable file")
	}
	return nil
}

func inspectHarness(setting protocol.HarnessSetting, mountedDir ...string) (protocol.HarnessRow, error) {
	command, ok := knownHarness(setting.Tool, setting.Variant)
	if !ok {
		return protocol.HarnessRow{}, errors.New("unknown harness")
	}
	row := protocol.HarnessRow{HarnessSetting: setting, Source: "auto"}
	if setting.OverridePath != "" {
		if err := executablePath(setting.OverridePath); err != nil {
			return row, err
		}
		row.LauncherPath = setting.OverridePath
		row.Source = "user"
	} else if path, err := exec.LookPath(command); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			row.LauncherPath = abs
		}
	}
	row.PolicyDisabled = false
	if row.LauncherPath == "" {
		row.UnavailableReason = "launcher not found on PATH"
		return row, nil
	}
	var versionErr error
	// Docker launchers require a mirrored working directory even for --version.
	// Discovery has no checkout yet; keep the PATH result and defer the version
	// probe unless a known mounted directory is supplied.
	if setting.Variant != "docker" || len(mountedDir) > 0 && mountedDir[0] != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, row.LauncherPath, "--version")
		if setting.Variant == "docker" {
			cmd.Dir = mountedDir[0]
			cmd.Env = append(os.Environ(), strings.ToUpper(setting.Tool)+"_LAUNCH_FLAGS=")
		}
		out, err := cmdprobe.Output(cmd) // a launcher that exits 0 and leaves a helper behind still reported its version
		versionErr = err
		if err == nil {
			row.Version = strings.TrimSpace(string(out))
		}
	}
	if !setting.Enabled {
		row.UnavailableReason = "disabled by owner"
	} else if setting.Variant == "native" && (versionErr != nil || row.Version == "") {
		row.UnavailableReason = "launcher version check failed"
	} else {
		row.Available = true
	}
	return row, nil
}

func defaultHarnesses() []protocol.HarnessRow {
	rows := make([]protocol.HarnessRow, 0, len(harnessNames))
	for _, item := range harnessNames {
		row, _ := inspectHarness(protocol.HarnessSetting{Tool: item.tool, Variant: item.variant, Enabled: true})
		rows = append(rows, row)
	}
	return rows
}

func (c *Client) setHarness(setting protocol.HarnessSetting) error {
	c.mu.Lock()
	probeDir := c.harnessProbeDir
	c.mu.Unlock()
	row, err := inspectHarness(setting, probeDir)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Harnesses {
		if c.Harnesses[i].Tool == setting.Tool && c.Harnesses[i].Variant == setting.Variant {
			c.Harnesses[i] = row
			return nil
		}
	}
	return errors.New("unknown harness")
}

func (c *Client) harnessRow(key string) protocol.HarnessRow {
	for _, row := range c.Harnesses {
		if harnessKey(row.Tool, row.Variant) == key {
			return row
		}
	}
	return protocol.HarnessRow{}
}
