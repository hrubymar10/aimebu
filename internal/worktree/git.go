package worktree

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// gitGuards are the settings every host-side git call on an agent-writable checkout runs with. A Docker agent has its worktree, and with it the
// repository's shared config, hooks and info/attributes, mounted, and git runs programs that a repository's own config names: core.fsmonitor
// on every status, hooks on commands such as `worktree add`, gpg.program when log.showSignature asks `git log` to check a signed commit. A
// planted one would run as the host user. Settings given to git in its environment as command-line config win over every config file.
//
// diff.ignoreSubmodules keeps a status out of submodules and embedded repositories: a nested repository has its own config, whose filter drivers
// gitFilterGuards never reads, so a status that went in would run them. A submodule's ignore setting in .gitmodules (a tracked file the agent
// writes) outranks it, so GitCommand also puts --ignore-submodules=all on every status, which nothing outranks. A dirty nested repository then
// does not make the checkout dirty.
var gitGuards = [][2]string{{"core.fsmonitor", "false"}, {"core.hooksPath", os.DevNull}, {"log.showSignature", "false"}, {"diff.ignoreSubmodules", "all"}}

// GitCommand is a git command run on the host in dir, a checkout an agent may be able to write to; every host-side git call on such a checkout
// is made through here. A status never goes into a nested repository (see gitGuards). Besides gitGuards, it takes from each filter driver the repository's own config (local or worktree scope, includes
// followed) defines or changes the commands that scope gave it: a status runs a file's clean filter when it must read the file, and a checkout
// its smudge filter. Such a driver keeps the clean, smudge, process and required values the trusted scopes (system, global, command line) give
// it, and nothing where they give none (a process the repository adds to a driver they give none turns the driver off, since git runs no clean
// or smudge command of a driver with an empty process); a driver only those scopes define (git-lfs installed globally, say) is left alone. The drivers are read
// right before the command, so one an agent adds between the two is not covered. Optional locks are off, so a status never takes the index lock
// from under the agent, and git never prompts.
//
// The system config is kept (GIT_CONFIG_NOSYSTEM is not set): an agent cannot write it, and dropping it could drop a host-wide setting
// (safe.directory, for one) that the host's git relies on.
func GitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	if len(args) > 0 && args[0] == "status" {
		args = append([]string{"status", "--ignore-submodules=all"}, args[1:]...)
	}
	return guardedGit(ctx, dir, append(append([][2]string(nil), gitGuards...), gitFilterGuards(ctx, dir)...), args...)
}

func guardedGit(ctx context.Context, dir string, guards [][2]string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = withGitConfig(os.Environ(), guards)
	return cmd
}

// gitFilterGuards reads every filter.* setting of dir's config with its scope (reading config runs nothing) and answers the settings that put
// back, for each driver the repository's own scopes touch, the trusted scopes' values (see GitCommand).
func gitFilterGuards(ctx context.Context, dir string) [][2]string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := guardedGit(ctx, dir, gitGuards, "config", "-z", "--show-scope", "--get-regexp", `^filter\.`)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil // none defined (exit 1), or not a repository: the command itself says what is wrong
	}
	trusted := map[string]string{}
	local := map[string]map[string]bool{} // the fields each driver's repository scopes set
	var names []string
	records := bytes.Split(out, []byte{0})
	for i := 0; i+1 < len(records); i += 2 {
		scope := string(records[i])
		key, value, _ := strings.Cut(string(records[i+1]), "\n")
		rest, ok := strings.CutPrefix(key, "filter.")
		dot := strings.LastIndexByte(rest, '.')
		if !ok || dot <= 0 {
			continue
		}
		if scope != "local" && scope != "worktree" {
			trusted[key] = value // the last value wins, as in git
			continue
		}
		name := rest[:dot]
		if local[name] == nil {
			local[name] = map[string]bool{}
			names = append(names, name)
		}
		local[name][rest[dot+1:]] = true
	}
	var guards [][2]string
	for _, name := range names {
		for _, field := range []string{"clean", "smudge", "required", "process"} {
			key := "filter." + name + "." + field
			value, ok := trusted[key]
			switch {
			case ok:
			case field == "required":
				value = "false"
			case field == "process" && !local[name]["process"]:
				// An empty process command is not "no process": git then runs neither clean nor smudge. It is set only to stop a process the
				// repository gave a driver the trusted scopes do not give one, which turns that driver off as a whole.
				continue
			}
			guards = append(guards, [2]string{key, value})
		}
	}
	return guards
}

// withGitConfig adds settings to env as git's command-line config (GIT_CONFIG_COUNT, GIT_CONFIG_KEY_<n>, GIT_CONFIG_VALUE_<n>), after any the
// environment already carries, which keeps a key and its value apart whatever characters a driver's name has. It also turns optional locks and
// prompts off.
func withGitConfig(env []string, settings [][2]string) []string {
	out := make([]string, 0, len(env)+2*len(settings)+3)
	count := 0
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "GIT_CONFIG_COUNT="); ok {
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				count = n
			}
			continue
		}
		out = append(out, entry)
	}
	for i, setting := range settings {
		n := strconv.Itoa(count + i)
		out = append(out, "GIT_CONFIG_KEY_"+n+"="+setting[0], "GIT_CONFIG_VALUE_"+n+"="+setting[1])
	}
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(count+len(settings)), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
}
