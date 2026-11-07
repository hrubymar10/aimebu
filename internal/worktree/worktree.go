package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Result struct {
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseRef    string `json:"base_ref"`
	BaseCommit string `json:"base_commit"`
	DirtyBase  int    `json:"dirty_base_count,omitempty"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	// Inspect fills these in; the other operations leave them empty.
	HeadCommit   string `json:"head_commit,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	DirtyFiles   int    `json:"dirty_files,omitempty"`
	Merged       bool   `json:"merged,omitempty"`
	LastActivity string `json:"last_activity,omitempty"`
}

type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string           { return e.Err.Error() }
func (e *Error) Unwrap() error           { return e.Err }
func problem(code, message string) error { return &Error{Code: code, Err: errors.New(message)} }

func git(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func repo(ctx context.Context, root string) (string, string, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", problem("not_git_repo", "project folder does not exist")
	}
	canonical = filepath.Clean(canonical)
	top, err := git(ctx, canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", notUsable(err)
	}
	resolvedTop, err := filepath.EvalSymlinks(top)
	if err != nil || filepath.Clean(resolvedTop) != canonical {
		return "", "", problem("not_git_root", "project folder must be the Git worktree root")
	}
	exclude, err := git(ctx, canonical, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(canonical, exclude)
	}
	return canonical, exclude, nil
}

// notUsable says why git refused the folder. "Not a repository" is the common answer, but git also refuses a repository it does not trust
// (dubious ownership) or cannot read, and calling that "not a Git repository" sends the owner looking in the wrong place: those say what
// git said.
func notUsable(err error) error {
	text := err.Error()
	if errors.Is(err, exec.ErrNotFound) {
		return problem("git_unusable", "Git is not installed on this machine")
	}
	if strings.Contains(text, "not a git repository") {
		return problem("not_git_repo", "project folder is not a Git repository")
	}
	detail := text
	if i := strings.LastIndex(detail, ": fatal: "); i >= 0 {
		detail = detail[i+len(": fatal: "):]
	} else if i := strings.LastIndex(detail, ": "); i >= 0 {
		detail = detail[i+2:]
	}
	return problem("git_unusable", "Git cannot use the project folder: "+strings.TrimSpace(detail))
}

// Validate checks the mapped project root before a room opts into worktrees.
func Validate(ctx context.Context, root string) error {
	_, _, err := repo(ctx, root)
	return err
}

func locked(exclude string, fn func() (Result, error)) (Result, error) {
	if err := os.MkdirAll(filepath.Dir(exclude), 0700); err != nil {
		return Result{}, err
	}
	lockPath := filepath.Join(filepath.Dir(exclude), "aimebu-worktrees.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return Result{}, err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func ensureDirectories(root string) error {
	for _, path := range []string{filepath.Join(root, ".aimebu"), filepath.Join(root, ".aimebu", "worktrees")} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(path, 0700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return problem("unsafe_path", ".aimebu worktree parent must be a real directory")
		}
	}
	return nil
}

func ensureExclude(exclude string) error {
	data, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == ".aimebu/" {
			return nil
		}
	}
	file, err := os.OpenFile(exclude, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		if _, err := file.WriteString("\n"); err != nil {
			return err
		}
	}
	if _, err := file.WriteString(".aimebu/\n"); err != nil {
		return err
	}
	return file.Sync()
}

func pathAndBranch(root, roomID string) (string, string, error) {
	if !strings.HasPrefix(roomID, "room_") || strings.ContainsAny(roomID, `/\\.`) {
		return "", "", problem("invalid_room", "invalid room ID for worktree")
	}
	return filepath.Join(root, ".aimebu", "worktrees", roomID), "aimebu/" + roomID, nil
}

func commonDir(ctx context.Context, root string) (string, error) {
	path, err := git(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return filepath.EvalSymlinks(path)
}

func dirtyCount(ctx context.Context, root string, ignored bool) (int, error) {
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}
	if ignored {
		args = append(args, "--ignored")
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	entries := bytes.Split(output, []byte{0})
	count := 0
	for i := 0; i < len(entries) && len(entries[i]) > 0; i++ {
		entry := entries[i]
		if len(entry) < 3 {
			return 0, errors.New("malformed Git status output")
		}
		// Adapter runtime files are created by aimebu inside each worktree.
		// Tracked changes still count, even if their path is under .aimebu.
		if !(ignored && entry[0] == '!' && entry[1] == '!' &&
			(strings.HasPrefix(string(entry[3:]), ".aimebu/") || string(entry[3:]) == ".aimebu")) {
			count++
		}
		if entry[0] == 'R' || entry[1] == 'R' || entry[0] == 'C' || entry[1] == 'C' {
			i++ // Porcelain -z adds the original path after a rename or copy.
		}
	}
	return count, nil
}

func Ensure(ctx context.Context, root, roomID, baseRef string) (Result, error) {
	root, exclude, err := repo(ctx, root)
	if err != nil {
		return Result{}, err
	}
	return locked(exclude, func() (Result, error) {
		path, branch, err := pathAndBranch(root, roomID)
		if err != nil {
			return Result{}, err
		}
		if baseRef == "" {
			baseRef, err = git(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
			if err != nil {
				baseRef = "HEAD"
			}
		}
		_, branchErr := git(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
		commit, err := git(ctx, root, "rev-parse", "--verify", "--end-of-options", baseRef+"^{commit}")
		if err != nil {
			if branchErr != nil {
				return Result{}, problem("invalid_base", "base ref must name a committed revision")
			}
			commit, err = git(ctx, root, "rev-parse", "--verify", "--end-of-options", branch+"^{commit}")
			if err != nil {
				return Result{}, err
			}
		}
		if err := ensureDirectories(root); err != nil {
			return Result{}, err
		}
		if err := ensureExclude(exclude); err != nil {
			return Result{}, err
		}
		dirty, err := dirtyCount(ctx, root, false)
		if err != nil {
			return Result{}, err
		}
		result := Result{Path: path, Branch: branch, BaseRef: baseRef, BaseCommit: commit, DirtyBase: dirty, State: "ready"}
		if info, err := os.Lstat(path); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return Result{}, problem("path_exists", "worktree path is occupied")
			}
			actual, err := git(ctx, path, "rev-parse", "--show-toplevel")
			if err != nil || filepath.Clean(actual) != path {
				return Result{}, problem("path_exists", "worktree path is occupied by another directory")
			}
			current, err := git(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
			if err != nil || current != branch {
				return Result{}, problem("branch_conflict", "worktree path uses another branch")
			}
			rootCommon, rootErr := commonDir(ctx, root)
			pathCommon, pathErr := commonDir(ctx, path)
			if rootErr != nil || pathErr != nil || rootCommon != pathCommon {
				return Result{}, problem("path_exists", "worktree path belongs to another repository")
			}
			return result, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		if branchErr == nil {
			if _, err := git(ctx, root, "worktree", "add", "--", path, branch); err != nil {
				return Result{}, problem("branch_conflict", err.Error())
			}
		} else {
			if _, err := git(ctx, root, "worktree", "add", "-b", branch, "--", path, commit); err != nil {
				return Result{}, problem("worktree_create_failed", err.Error())
			}
		}
		return result, nil
	})
}

func Remove(ctx context.Context, root, roomID, baseRef string, force bool) (Result, error) {
	root, exclude, err := repo(ctx, root)
	if err != nil {
		return Result{}, err
	}
	return locked(exclude, func() (Result, error) {
		path, branch, err := pathAndBranch(root, roomID)
		if err != nil {
			return Result{}, err
		}
		result := Result{Path: path, Branch: branch, BaseRef: baseRef, State: "removed"}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		} else if err != nil {
			return Result{}, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return Result{}, problem("unsafe_path", "worktree path must be a real directory")
		}
		current, err := git(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil || current != branch {
			return Result{}, problem("branch_conflict", "worktree path does not use the room branch")
		}
		rootCommon, rootErr := commonDir(ctx, root)
		pathCommon, pathErr := commonDir(ctx, path)
		if rootErr != nil || pathErr != nil || rootCommon != pathCommon {
			return Result{}, problem("path_exists", "worktree path belongs to another repository")
		}
		if !force {
			dirty, err := dirtyCount(ctx, path, true)
			if err != nil {
				return Result{}, err
			}
			if dirty > 0 {
				result.State, result.Reason = "preserved", fmt.Sprintf("%d uncommitted or generated files", dirty)
				return result, nil
			}
			if baseRef == "" || baseRef == "HEAD" {
				result.State, result.Reason = "preserved", "base branch is unknown"
				return result, nil
			}
			if _, err := git(ctx, root, "merge-base", "--is-ancestor", "--", branch, baseRef); err != nil {
				result.State, result.Reason = "preserved", "room branch has unmerged commits"
				return result, nil
			}
		}
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		args = append(args, "--", path)
		if _, err := git(ctx, root, args...); err != nil {
			return Result{}, problem("worktree_remove_failed", err.Error())
		}
		return result, nil
	})
}

// Retire sets the branch of a worktree that is gone aside under a name nothing will use again, so the name it had can be given to someone else: the work
// on it is kept, and a worktree made later under the same name starts from the base and not from these commits. It renames aimebu/<name> to
// aimebu/<name>-removed-<n>, n being the first number not taken, and reports the new name in Branch (empty when there was no branch to set aside).
// It refuses while a worktree still stands at the name's path, and it is safe to call again.
func Retire(ctx context.Context, root, roomID string) (Result, error) {
	root, exclude, err := repo(ctx, root)
	if err != nil {
		return Result{}, err
	}
	return locked(exclude, func() (Result, error) {
		path, branch, err := pathAndBranch(root, roomID)
		if err != nil {
			return Result{}, err
		}
		result := Result{Path: path, State: "removed"}
		if _, err := os.Lstat(path); err == nil {
			return Result{}, problem("worktree_exists", "the worktree is still there: remove it before its branch is set aside")
		} else if !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		if _, err := git(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
			return result, nil // no branch: nothing to set aside
		}
		for n := 1; n < 1000; n++ {
			next := fmt.Sprintf("%s-removed-%d", branch, n)
			if _, err := git(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+next); err == nil {
				continue
			}
			if _, err := git(ctx, root, "branch", "-m", "--", branch, next); err != nil {
				return Result{}, problem("branch_rename_failed", err.Error())
			}
			result.Branch = next
			return result, nil
		}
		return Result{}, problem("branch_rename_failed", "no free name to set the branch aside under")
	})
}

// Inspect reports a room worktree without changing anything: whether it exists, how big it is, whether it has uncommitted or generated
// files, where its branch stands against baseRef, and when it was last touched. It is what the cleanup decides on.
func Inspect(ctx context.Context, root, roomID, baseRef string) (Result, error) {
	root, _, err := repo(ctx, root)
	if err != nil {
		return Result{}, err
	}
	path, branch, err := pathAndBranch(root, roomID)
	if err != nil {
		return Result{}, err
	}
	result := Result{Path: path, Branch: branch, BaseRef: baseRef, State: "removed"}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	} else if err != nil {
		return Result{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Result{}, problem("unsafe_path", "worktree path must be a real directory")
	}
	current, err := git(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || current != branch {
		return Result{}, problem("branch_conflict", "worktree path does not use the room branch")
	}
	rootCommon, rootErr := commonDir(ctx, root)
	pathCommon, pathErr := commonDir(ctx, path)
	if rootErr != nil || pathErr != nil || rootCommon != pathCommon {
		return Result{}, problem("path_exists", "worktree path belongs to another repository")
	}
	result.State = "ready"
	if result.HeadCommit, err = git(ctx, path, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return Result{}, err
	}
	if result.DirtyFiles, err = dirtyCount(ctx, path, true); err != nil {
		return Result{}, err
	}
	if baseRef != "" && baseRef != "HEAD" {
		_, mergeErr := git(ctx, root, "merge-base", "--is-ancestor", "--", branch, baseRef)
		result.Merged = mergeErr == nil
	}
	latest := time.Time{}
	if stamp, err := git(ctx, path, "log", "-1", "--format=%ct", "HEAD"); err == nil {
		if seconds, err := strconv.ParseInt(stamp, 10, 64); err == nil {
			latest = time.Unix(seconds, 0)
		}
	}
	err = filepath.WalkDir(path, func(entry string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // a file that vanished mid-walk is not an error
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir // the worktree's own .git is a file; a nested repository's history is not this room's work
		}
		if fi, err := d.Info(); err == nil && !d.IsDir() {
			result.SizeBytes += fi.Size()
			if fi.ModTime().After(latest) {
				latest = fi.ModTime()
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if !latest.IsZero() {
		result.LastActivity = latest.UTC().Format(time.RFC3339)
	}
	return result, nil
}
