package updatecheck

import (
	"context"
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Status is what one check concluded. State is "available" (the newest release
// for the channel is newer than the running process), "installed" (the cached
// binary on disk was built from something newer than the running process, so a
// restart picks it up), "unbuilt" (the checkout moved past the running process
// but the cached binary was not rebuilt, so a restart alone changes nothing), or
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

// gitEnv is the environment for the check's git calls: fully non-interactive, as the
// wrapper's own (no credential, passphrase or host-key prompt; an ssh configured
// through GIT_SSH_COMMAND is left alone).
func gitEnv(base []string) []string {
	ssh := "ssh -o BatchMode=yes"
	for _, entry := range base {
		if value, ok := strings.CutPrefix(entry, "GIT_SSH_COMMAND="); ok && value != "" {
			ssh = value
		}
	}
	return append(base, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "SSH_ASKPASS=", "GIT_SSH_COMMAND="+ssh)
}

// gitOut runs one git command in the clone with a time limit and returns stdout.
func gitOut(dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(os.Environ())
	// A killed git can leave an ssh child holding the output pipe; do not wait on it.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// gitOK reports whether a git command in the clone exits 0 (merge-base --is-ancestor
// and cat-file -e answer through the exit code).
func gitOK(dir string, args ...string) bool {
	_, err := gitOut(dir, 10*time.Second, args...)
	return err == nil
}

// Check runs one update check against the clone layout. runningVersion is the
// version string the binary was built with ("aimebu v2.0.0" style, possibly a
// `git describe` answer such as v2.0.0-3-gabcdef); started is when this process
// started, so a binary rebuilt after it counts as installed. remoteName is the git
// remote to follow (empty means origin).
//
// The check offers only what `aimebu update` would do: a dirty checkout, a named
// branch with no channel set (never detached onto a tag), and a checkout carrying
// commits the target tag does not have are all left alone, and a clone on the
// remote's default branch with no channel keeps following that branch.
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
	head, err := gitOut(layout.Dir, 10*time.Second, "rev-parse", "HEAD")
	if err != nil {
		return status, err
	}
	channel := channelFromEnvironment(configRoot)
	// "installed" is about the built binary only: its embedded version (or VCS
	// revision) says what a restart would run. A checkout that moved while the
	// binary did not (a manual `git pull`) is "unbuilt": restarting changes nothing
	// until the binary is rebuilt.
	if installed, built := binaryMovedPast(layout, started, version); installed {
		status.State, status.Version, status.Channel = "installed", built, channel
		return status, nil
	}
	if versionMovedPast(layout.Dir, describe, head, version) {
		status.State, status.Version, status.Channel = "unbuilt", describe, channel
		return status, nil
	}
	if channel == Edge || strings.HasPrefix(channel, Edge+":") {
		// The branch channels are the wrapper's business; the server only reports
		// release tags, so there is nothing to say here.
		return status, nil
	}
	// The wrapper refuses a dirty checkout, so offering it an update would be a lie.
	if dirty, err := gitOut(layout.Dir, 10*time.Second, "status", "--porcelain", "--untracked-files=no"); err != nil || dirty != "" {
		return status, err
	}
	if channel == "" {
		branch, _ := gitOut(layout.Dir, 10*time.Second, "symbolic-ref", "--quiet", "--short", "HEAD")
		if branch != "" {
			// No channel and a named branch. On the remote's default branch the wrapper
			// keeps its old behaviour (follow the branch); on any other branch it only
			// warns and never detaches, so there is nothing to offer.
			return checkDefaultBranch(layout.Dir, remoteName, branch, head, status)
		}
	}
	out, err := gitOut(layout.Dir, 10*time.Second, "ls-remote", "--tags", remoteName)
	if err != nil {
		return status, err
	}
	// ls-remote answers a peeled "refs/tags/<name>^{}" line only for annotated
	// tags, so the peeled commit marks the annotation just as it does for the
	// wrapper and is the commit the wrapper would land on. (--refs would filter
	// exactly those lines out, so it is not used.)
	var tags []string
	commits := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasSuffix(fields[1], "^{}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(fields[1], "refs/tags/"), "^{}")
		if _, ok := ParseTag(name); ok {
			tags = append(tags, name)
			commits[name] = fields[0]
		}
	}
	if channel == "" {
		channel = DefaultChannel(tags, version)
	}
	target, ok := SelectTag(tags, version, channel)
	if !ok {
		return status, nil // no-release: nothing to offer
	}
	if newer, ok := IsNewer(target, version); !ok || !newer {
		return status, nil
	}
	// A checkout with commits the target does not have is diverged: the wrapper
	// leaves it alone. When the target commit is not fetched yet, divergence cannot
	// be proven and the wrapper re-checks with the tag in hand, so the offer stands.
	if oid := commits[target]; gitOK(layout.Dir, "cat-file", "-e", oid+"^{commit}") && !gitOK(layout.Dir, "merge-base", "--is-ancestor", head, oid) {
		return status, nil
	}
	status.State, status.Version, status.Channel = "available", target, channel
	return status, nil
}

// versionMovedPast reports whether version names something newer than the running
// process: a newer tag, the same tag further along, or (same tag and distance) a
// different commit that is a descendant of the running build's commit. sha, when
// known, is the full commit version was taken at.
func versionMovedPast(dir, version, sha, running string) bool {
	c, ok := CompareVersions(version, running)
	if !ok {
		return false
	}
	if c != 0 {
		return c > 0
	}
	next, current := parsedDescribe(version), parsedDescribe(running)
	if sha == "" {
		sha = next.SHA
	}
	if next.Count == 0 || current.SHA == "" || sha == "" || strings.HasPrefix(sha, current.SHA) || strings.HasPrefix(current.SHA, sha) {
		return false
	}
	return gitOK(dir, "merge-base", "--is-ancestor", current.SHA, sha)
}

func parsedDescribe(version string) Describe {
	d, _ := ParseDescribe(version)
	return d
}

var ldflagsVersion = regexp.MustCompile(`main\.version=aimebu ([^'" ]+)`)

// builtVersion reads what the binary at path was built from: the version the
// wrapper stamped with -ldflags, else its VCS revision. Both are empty when the
// file is not a Go binary.
func builtVersion(path string) (version, revision string) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", ""
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "-ldflags":
			if m := ldflagsVersion.FindStringSubmatch(setting.Value); m != nil {
				version = m[1]
			}
		case "vcs.revision":
			revision = setting.Value
		}
	}
	return version, revision
}

// runningSHA finds the commit in the running build's version string: the describe
// suffix, or a bare revision field ("aimebu v2 abc123def456 ...").
func runningSHA(running string) string {
	if d, ok := ParseDescribe(running); ok && d.SHA != "" {
		return d.SHA
	}
	for _, field := range strings.Fields(running) {
		if len(field) >= 7 && isHex(field) {
			return field
		}
	}
	return ""
}

// binaryMovedPast reports whether the cached binary on disk was built from
// something newer than the running process, and what that is. A binary no newer
// than the process start cannot be a rebuild, so it is not even read.
func binaryMovedPast(layout CloneLayout, started time.Time, running string) (bool, string) {
	info, err := os.Stat(layout.Binary)
	if err != nil || !info.ModTime().After(started) {
		return false, ""
	}
	version, revision := builtVersion(layout.Binary)
	if version != "" {
		return versionMovedPast(layout.Dir, version, "", running), version
	}
	if sha := runningSHA(running); revision != "" && sha != "" && !strings.HasPrefix(revision, sha) {
		short := revision
		if len(short) > 12 {
			short = short[:12]
		}
		return gitOK(layout.Dir, "merge-base", "--is-ancestor", sha, revision), short
	}
	return false, ""
}

// checkDefaultBranch is the no-channel case on a named branch. Only the remote's
// default branch is followed (the wrapper's v1 behaviour); the version it reports
// is the short remote commit it would fast-forward to.
func checkDefaultBranch(dir, remote, branch, head string, status Status) (Status, error) {
	out, err := gitOut(dir, 10*time.Second, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return status, err
	}
	var ref, oid string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 3 && fields[0] == "ref:" && fields[2] == "HEAD":
			ref = fields[1]
		case len(fields) == 2 && fields[1] == "HEAD":
			oid = fields[0]
		}
	}
	if ref != "refs/heads/"+branch || oid == "" || oid == head {
		return status, nil
	}
	// Behind means HEAD is an ancestor of the remote tip. A tip already in the
	// checkout leaves nothing to fetch, and a checkout with commits of its own that
	// the tip lacks is diverged: the wrapper leaves it alone, so nothing is offered.
	// A tip that is not fetched yet cannot be judged, and is behind as for the wrapper.
	if gitOK(dir, "cat-file", "-e", oid+"^{commit}") && !gitOK(dir, "merge-base", "--is-ancestor", head, oid) {
		return status, nil
	}
	short := oid
	if len(short) > 12 {
		short = short[:12]
	}
	status.State, status.Version, status.Channel = "available", short, "default-branch"
	return status, nil
}

// IsNewer reports whether candidate names a release strictly newer than running
// (both read as describe-style versions; a plain commit hash or an unparseable
// version never is). The second result is false when either side is not a release
// version.
func IsNewer(candidate, running string) (bool, bool) {
	c, ok := CompareVersions(candidate, running)
	return c > 0, ok
}
