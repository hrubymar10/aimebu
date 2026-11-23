package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/worktree"
)

// workTreeWait bounds the git calls that describe a checkout after a limit kill: the exit is reported without the description rather than late.
var workTreeWait = 5 * time.Second

// workTreeSubjectMax bounds the commit subject kept in the description.
const workTreeSubjectMax = 120

// workTreeState describes the checkout a turn worked in, for the evidence of a turn-limit kill: "tip ab31772 <subject>, tree clean" or
// "tip ab31772 <subject>, 3 dirty files", so whoever picks the continuation up (the agent, a dispatcher reading the room) sees at once whether
// the killed turn had committed its work. It is "" when the folder is not a git checkout, or git fails or takes longer than workTreeWait. Git
// runs through worktree.GitCommand: with optional locks off, so the status never takes the index lock from under the agent's next turn, and
// with the programs a repository's config can name (fsmonitor, hooks, filters) off, since the agent can write that config.
func workTreeState(ctx context.Context, dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, workTreeWait)
	defer cancel()
	git := func(args ...string) (string, bool) {
		cmd := worktree.GitCommand(ctx, dir, args...)
		var out bytes.Buffer
		cmd.Stdout = &out
		if cmd.Run() != nil {
			return "", false
		}
		return out.String(), true
	}
	tip, ok := git("log", "-1", "--oneline", "--no-decorate", "--no-color")
	if !ok {
		return ""
	}
	tip = strings.ToValidUTF8(strings.TrimSpace(tip), "")
	if len(tip) > workTreeSubjectMax {
		tip = strings.ToValidUTF8(tip[:workTreeSubjectMax], "") + "…"
	}
	status, ok := git("status", "--porcelain", "--untracked-files=normal")
	if !ok {
		return ""
	}
	dirty := 0
	for _, line := range strings.Split(status, "\n") {
		if strings.TrimSpace(line) != "" {
			dirty++
		}
	}
	tree := "tree clean"
	switch {
	case dirty == 1:
		tree = "1 dirty file"
	case dirty > 1:
		tree = fmt.Sprintf("%d dirty files", dirty)
	}
	return "tip " + tip + ", " + tree
}
