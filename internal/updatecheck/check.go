package updatecheck

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Status is what one check concluded. State is "available" (the newest release
// for the channel is newer than the running process), "installed" (the clone or
// the binary is newer than the running process, so a restart picks it up), or
// empty (nothing to do).
type Status struct {
	State     string
	Version   string // the tag or checkout version the state is about
	Channel   string
	CheckedAt time.Time
}

// CloneLayout describes where the pieces of a wrapper install live.
type CloneLayout struct {
	Dir    string // the clone root (the directory that holds .git and bin/)
	Binary string // the cached binary the wrapper built
}

// LayoutFromExecutable finds the clone layout around the running binary: the
// clone is detectable only when <dir of os.Executable()>/.git plus bin/aimebu
// exist. It reports false for a bare `go install` binary or any other install
// that is not a wrapper clone.
func LayoutFromExecutable() (CloneLayout, bool) {
	exe, err := os.Executable()
	if err != nil {
		return CloneLayout{}, false
	}
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return CloneLayout{}, false
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "aimebu")); err != nil {
		return CloneLayout{}, false
	}
	return CloneLayout{Dir: dir, Binary: exe}, true
}

// channelFromEnvironment resolves the channel the way the wrapper does:
// AIMEBU_UPDATE_CHANNEL, else the one-word <config root>/update-channel file,
// else nothing. configRoot may be empty (the caller's default root is then
// ~/.aimebu under its own HOME).
func channelFromEnvironment(configRoot string) string {
	if word := os.Getenv("AIMEBU_UPDATE_CHANNEL"); word != "" {
		word, _ = ParseChannel(strings.TrimSpace(word))
		return word
	}
	if configRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configRoot = filepath.Join(home, ".aimebu")
	}
	raw, err := os.ReadFile(filepath.Join(configRoot, "update-channel"))
	if err != nil {
		return ""
	}
	word, _ := ParseChannel(strings.TrimSpace(string(raw)))
	return word
}

// gitOut runs one git command in the clone with a time limit and returns stdout.
func gitOut(dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Check runs one update check against the clone layout. runningVersion is the
// version string the binary was built with ("aimebu v2.0.0" style); started is
// when this process started, so a binary rebuilt after it counts as installed.
// remoteNames the git remote to follow (empty means origin).
func Check(layout CloneLayout, runningVersion string, started time.Time, configRoot, remoteName string) (Status, error) {
	status := Status{CheckedAt: time.Now().UTC()}
	if remoteName == "" {
		remoteName = "origin"
	}
	version := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(runningVersion), "aimebu"))
	describe, err := gitOut(layout.Dir, 10*time.Second, "describe", "--tags", "--always")
	if err != nil {
		return status, err
	}
	channel := channelFromEnvironment(configRoot)
	// A clone that has moved past the running build means the update was applied
	// and a restart picks it up.
	if newer, ok := IsNewer(describe, version); ok && newer {
		status.State, status.Version, status.Channel = "installed", describe, channel
		return status, nil
	}
	if info, err := os.Stat(layout.Binary); err == nil && info.ModTime().After(started) {
		status.State, status.Version, status.Channel = "installed", describe, channel
		return status, nil
	}
	if channel == Edge || strings.HasPrefix(channel, Edge+":") {
		// The branch channels are the wrapper's business; the server only reports
		// release tags, so there is nothing to say here.
		return status, nil
	}
	out, err := gitOut(layout.Dir, 10*time.Second, "ls-remote", "--tags", remoteName)
	if err != nil {
		return status, err
	}
	// ls-remote answers a peeled "refs/tags/<name>^{}" line only for annotated
	// tags, so the peeled commit marks the annotation just as it does for the
	// wrapper; the tag object itself is never needed here. (--refs would filter
	// exactly those lines out, so it is not used.)
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasSuffix(fields[1], "^{}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(fields[1], "refs/tags/"), "^{}")
		if name != "" {
			tags = append(tags, name)
		}
	}
	if channel == "" {
		channel = DefaultChannel(tags, version)
	}
	target, ok := SelectTag(tags, version, channel)
	if !ok {
		return status, nil // no-release: nothing to offer
	}
	newer, ok := IsNewer(target, version)
	if ok && newer {
		status.State, status.Version, status.Channel = "available", target, channel
	}
	return status, nil
}

// IsNewer reports whether candidate names a release strictly newer than
// running (a plain commit hash or an unparseable version never is).
func IsNewer(candidate, running string) (bool, bool) {
	_, cOK := ParseTag(candidate)
	_, rOK := ParseTag(running)
	if !cOK || !rOK {
		return false, false
	}
	return CompareTags(candidate, running) > 0, true
}
