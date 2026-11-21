package updatecheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The server-side check runs real git against a local bare remote holding the
// shared tag table, so its parsing and channel rules are pinned the same way
// the wrapper's are.

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// checkWorld builds a bare remote with the shared tags on successive commits, a clone
// of it, and a stand-in cached binary.
func checkWorld(t *testing.T) (clone string, remote string, binary string) {
	t.Helper()
	for _, tool := range []string{"git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	// The dashed upload-pack/receive-pack symlinks do not resolve on every machine;
	// a private exec path carries shims that call the built-ins the transport would
	// have reached.
	shim := t.TempDir()
	for name, sub := range map[string]string{"git-upload-pack": "upload-pack", "git-receive-pack": "receive-pack"} {
		if err := os.WriteFile(filepath.Join(shim, name), []byte("#!/bin/sh\nexec git "+sub+" \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_EXEC_PATH", shim)
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	gitRun(t, base, "init", "-q", "--bare", remote)
	work := filepath.Join(base, "work")
	gitRun(t, base, "clone", "-q", remote, work)
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module standin\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "one")
	gitRun(t, work, "tag", "-a", "v2.0.0", "-m", "v2.0.0")
	// One commit and tag per remaining shared tag, oldest to newest.
	for _, tag := range TestTags {
		if tag == "v2.0.0" {
			continue
		}
		if err := os.WriteFile(filepath.Join(work, "marker"), []byte(tag), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, work, "add", "-A")
		gitRun(t, work, "commit", "-q", "-m", "publish "+tag)
		gitRun(t, work, "tag", "-a", tag, "-m", tag)
	}
	gitRun(t, work, "push", "-q", "origin", "master")
	gitRun(t, work, "push", "-q", "origin", "--tags")
	clone = filepath.Join(base, "clone")
	gitRun(t, base, "clone", "-q", remote, clone)
	// The clone knows the tags only once it fetched them, as a wrapper install has.
	gitRun(t, clone, "fetch", "-q", "--tags", "origin")
	binary = filepath.Join(clone, "aimebu-test")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The cache predates the checks by an hour, so only a moved clone or a rebuilt
	// binary reads as installed.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(binary, past, past); err != nil {
		t.Fatal(err)
	}
	return clone, remote, binary
}

func TestCheckFindsTheNewestStableRelease(t *testing.T) {
	clone, remote, binary := checkWorld(t)
	// A wrapper install sits on the release it runs, not on the branch tip.
	gitRun(t, clone, "checkout", "-q", "--detach", "v2.0.0")
	layout := CloneLayout{Dir: clone, Binary: binary}
	status, err := Check(layout, "aimebu v2.0.0", time.Now().Add(-time.Hour), "", remote)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "available" || status.Version != "v2.1.0" || status.Channel != "stable" {
		t.Fatalf("status = %+v, want available v2.1.0 stable", status)
	}
	// Current at the newest stable release: nothing to say.
	status, err = Check(layout, "aimebu v2.1.0", time.Now().Add(-time.Hour), "", remote)
	if err != nil || status.State != "" {
		t.Fatalf("current status = %+v, %v; want no state", status, err)
	}
}

func TestCheckFollowsTheChannelAndIgnoresLightweightTags(t *testing.T) {
	clone, remote, binary := checkWorld(t)
	gitRun(t, clone, "checkout", "-q", "--detach", "v2.0.0")
	layout := CloneLayout{Dir: clone, Binary: binary}
	// A lightweight tag is not a target, exactly as for the wrapper.
	if out, err := exec.Command("git", "-C", clone, "push", "-q", "origin", "HEAD:refs/tags/v2.9.9").CombinedOutput(); err != nil {
		t.Fatalf("push lightweight tag: %v\n%s", err, out)
	}
	status, err := Check(layout, "aimebu v2.0.0", time.Now().Add(-time.Hour), "", remote)
	if err != nil || status.Version != "v2.1.0" {
		t.Fatalf("lightweight tag became the target: %+v, %v", status, err)
	}
	// A prerelease of the next minor is on the remote: the stable channel ignores it,
	// the prerelease channel takes it.
	gitRun(t, clone, "tag", "-a", "v2.2.0-rc.1", "v2.1.0", "-m", "v2.2.0-rc.1")
	gitRun(t, clone, "push", "-q", "origin", "v2.2.0-rc.1")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "update-channel"), []byte("stable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU_UPDATE_CHANNEL", "")
	status, err = Check(layout, "aimebu v2.0.0", time.Now().Add(-time.Hour), root, remote)
	if err != nil || status.State != "available" || status.Version != "v2.1.0" || status.Channel != "stable" {
		t.Fatalf("stable status = %+v, %v", status, err)
	}
	if err := os.WriteFile(filepath.Join(root, "update-channel"), []byte("prerelease\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = Check(layout, "aimebu v2.0.0", time.Now().Add(-time.Hour), root, remote)
	if err != nil || status.State != "available" || status.Version != "v2.2.0-rc.1" || status.Channel != "prerelease" {
		t.Fatalf("prerelease status = %+v, %v", status, err)
	}
	// The variable beats the file, and an invalid word in the file counts as unset.
	t.Setenv("AIMEBU_UPDATE_CHANNEL", "edge")
	status, err = Check(layout, "aimebu v2.1.0", time.Now().Add(-time.Hour), root, remote)
	if err != nil || status.State != "" {
		t.Fatalf("edge status = %+v, %v; want no release-tag state", status, err)
	}
	if err := os.WriteFile(filepath.Join(root, "update-channel"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU_UPDATE_CHANNEL", "")
	status, err = Check(layout, "aimebu v2.0.0", time.Now().Add(-time.Hour), root, remote)
	if err != nil || status.State != "available" || status.Version != "v2.1.0" || status.Channel != "stable" {
		t.Fatalf("invalid channel status = %+v, %v; want the computed default", status, err)
	}
}

func TestCheckInstalledAndNoRelease(t *testing.T) {
	clone, remote, binary := checkWorld(t)
	layout := CloneLayout{Dir: clone, Binary: binary}
	// The clone has moved past the running build: installed, a restart picks it up.
	gitRun(t, clone, "checkout", "-q", "--detach", "v2.1.0")
	status, err := Check(layout, "aimebu v2.0.0", time.Now().Add(-time.Hour), "", remote)
	if err != nil || status.State != "installed" || status.Version != "v2.1.0" {
		t.Fatalf("installed status = %+v, %v", status, err)
	}
	// A binary rebuilt after this process started counts as installed too.
	now := time.Now()
	if err := os.Chtimes(binary, now, now); err != nil {
		t.Fatal(err)
	}
	status, err = Check(layout, "aimebu v2.1.0", time.Now().Add(-time.Hour), "", remote)
	if err != nil || status.State != "installed" {
		t.Fatalf("rebuilt binary status = %+v, %v", status, err)
	}
	// The cache is old again: nothing to say about the current release.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(binary, past, past); err != nil {
		t.Fatal(err)
	}
	// No release for the running major: nothing to offer.
	status, err = Check(layout, "aimebu v9.9.9", time.Now().Add(-time.Hour), "", remote)
	if err != nil || status.State != "" {
		t.Fatalf("no-release status = %+v, %v", status, err)
	}
	// A checkout that is not a wrapper clone has no layout at all.
	if _, ok := LayoutFromExecutable(); ok {
		t.Fatal("the test binary itself counted as a wrapper clone")
	}
	if !strings.Contains(filepath.Base(binary), "aimebu") {
		t.Fatal("setup broke")
	}
}
