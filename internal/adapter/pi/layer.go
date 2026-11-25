package pi

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/cmdprobe"
)

// layerScript builds a Docker turn's private Pi agent directory inside the container. Pi reads its providers, models, login, settings and
// packages from PI_CODING_AGENT_DIR, so pointing it at a directory holding only mcp.json leaves it with no model. Every entry of the container's
// own agent directory except mcp.json (written by the adapter, and the one thing the turn replaces) and sessions (the turn has its own
// --session-dir) is linked into the private one. A link, not a copy: Pi writes auth.json through a link (a refreshed login stays in the one
// file), and packages and caches stay shared. The directory is the one the container itself runs with, so no host path is guessed; and the
// links are rebuilt each turn, so an entry that appeared or vanished since the last one is followed.
const layerScript = `dst=$1
src="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}"
[ -d "$src" ] || { echo "no Pi agent directory at $src" >&2; exit 3; }
[ "$src" != "$dst" ] || { echo "the private Pi agent directory is the container's own" >&2; exit 4; }
mkdir -p "$dst" || exit 5
find "$dst" -maxdepth 1 -type l -exec rm -f {} + || exit 5
for e in "$src"/* "$src"/.[!.]*; do
  [ -e "$e" ] || [ -L "$e" ] || continue
  b=${e##*/}
  case "$b" in mcp.json|sessions) continue ;; esac
  ln -s "$e" "$dst/$b" || exit 6
done
`

// layerAgentDir runs layerScript through the launcher's command mode (HARNESS_DOCKER_RUN_COMMAND), in the checkout, with nothing forwarded
// into the container: no key, no toolbox. dir is the private directory as the container sees it.
func layerAgentDir(ctx context.Context, command string, args []string, checkout, dir string, refs map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = checkout
	cmd.Env = append(adapter.DockerControlEnv(refs), "HARNESS_DOCKER_RUN_COMMAND=sh -c "+shellQuote(layerScript)+" sh "+shellQuote(dir),
		"HARNESS_DOCKER_CWD=", "HARNESS_DOCKER_TIMEOUT_SECONDS=60")
	out, err := cmdprobe.CombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("Pi Docker agent directory: %s: %w", adapter.SanitizeStderr(string(out)), err)
	}
	return nil
}

// shellQuote makes one word of a value for sh: single quotes, with each quote in it closed, escaped and reopened.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// nativeLinked are the files of the person's own agent directory a native turn sees: the providers and models they defined, and their login.
// Their settings, extensions and packages stay out (the Ollama Cloud extension is loaded explicitly, see ollamaCloudExtension).
var nativeLinked = []string{"models.json", "auth.json"}

// linkUserFiles links each named file of the person's agent directory into a native turn's private one, replacing a link left by an earlier
// turn. A file the person does not have is skipped, and so is a source that is the private directory itself. Pi writes auth.json through a
// link, so a refreshed login lands in the person's own file.
func linkUserFiles(dir, source string, names ...string) error {
	if filepath.Clean(source) == filepath.Clean(dir) {
		return nil
	}
	for _, name := range names {
		target := filepath.Join(source, name)
		link := filepath.Join(dir, name)
		if info, err := os.Lstat(link); err == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				continue // a real file is the adapter's own, not a stale link
			}
			if err := os.Remove(link); err != nil {
				return err
			}
		}
		if _, err := os.Stat(target); err != nil {
			continue
		}
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	return nil
}
