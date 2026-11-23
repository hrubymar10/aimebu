package claude

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/ccsessions"
)

// launchConfigDir is the Claude state directory a process started with env will use: the last CLAUDE_CONFIG_DIR entry, else HOME/.claude.
func launchConfigDir(env []string) string {
	var configDir, home string
	for _, item := range env {
		if v, ok := strings.CutPrefix(item, "CLAUDE_CONFIG_DIR="); ok {
			configDir = v
		} else if v, ok := strings.CutPrefix(item, "HOME="); ok {
			home = v
		}
	}
	if configDir != "" {
		return configDir
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// transcriptExists says whether Claude's transcript of session id is in configDir: first the folder Claude would name for cwd (one stat),
// then one glob over the project folders. A read problem other than "not there" is returned.
func transcriptExists(configDir, cwd, id string) (bool, error) {
	if configDir == "" || !ccsessions.ValidID(id) {
		return false, errors.New("no usable config dir or session id")
	}
	name := id + ".jsonl"
	if cwd != "" {
		if _, err := os.Stat(filepath.Join(configDir, "projects", ccsessions.FolderName(cwd), name)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", name))
	if err != nil {
		return false, err
	}
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() {
			return true, nil
		}
	}
	return false, nil
}

// hostConfigDir is the host path of the Claude state directory this launch uses, or why none can be named. A native launch uses the
// environment's own. A Docker launcher that shares the host's Claude home uses the host's too; one that remaps CLAUDE_CONFIG_DIR to a path
// inside its container is mapped back through the launcher's path maps (the longest matching container prefix).
func hostConfigDir(spec adapter.TurnSpec, docker bool, env []string) (string, string) {
	if docker {
		return "", "the docker variant has no path map to the container's config dir"
	}
	if !spec.LauncherContainerized {
		return launchConfigDir(env), ""
	}
	if _, remapped := spec.LauncherEnvRefs["CLAUDE_CONFIG_DIR"]; remapped {
		visible := ""
		for _, item := range env {
			if v, ok := strings.CutPrefix(item, "CLAUDE_CONFIG_DIR="); ok {
				visible = v
			}
		}
		if visible == "" {
			return "", "the launcher's CLAUDE_CONFIG_DIR reference has no value"
		}
		best, host := -1, ""
		for _, m := range spec.LauncherPathMaps {
			rel, err := filepath.Rel(m.Visible, visible)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || len(m.Visible) <= best {
				continue
			}
			best, host = len(m.Visible), filepath.Join(m.Host, rel)
		}
		if host == "" {
			return "", "the container's config dir is not under a launcher path map"
		}
		return host, ""
	}
	if spec.LauncherSharesClaudeHome {
		return launchConfigDir(env), ""
	}
	return "", "the launcher does not share the host's Claude home"
}

// resumeOrFresh decides whether a turn continues its session with --resume, from whether the transcript is in the config dir this launch
// uses rather than from the started flag alone: --resume fails when there is no transcript (an account switch moved the agent to another
// config dir, or the first turn died before Claude wrote one) and --session-id collides when there is. The id never changes, so the
// server's record stays right. A fork (which must resume), a launch whose host config dir cannot be named, and any read error keep the
// flag; the last two are logged.
func (a *Adapter) resumeOrFresh(spec adapter.TurnSpec, env []string, cwd string) bool {
	handle, started := spec.NativeSessionHandle, spec.NativeSessionStarted
	if handle == "" || spec.ForkSession {
		return started
	}
	dir, why := hostConfigDir(spec, a.variant() == "docker", env)
	if why != "" {
		log.Printf("claude: not checking the transcript of session %s: %s; keeping started=%t", handle, why, started)
		return started
	}
	exists, err := transcriptExists(dir, cwd, handle)
	if err != nil {
		log.Printf("claude: could not check the transcript of session %s (%v); keeping started=%t", handle, err, started)
		return started
	}
	switch {
	case started && !exists:
		log.Printf("claude: session %s has no transcript in this launch's config dir; starting it fresh with the same id instead of --resume", handle)
		return false
	case !started && exists:
		log.Printf("claude: session %s already has a transcript in this launch's config dir; resuming it instead of --session-id", handle)
		return true
	}
	return started
}
