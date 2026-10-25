package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hrubymar10/aimebu/internal/store"
)

// pruneCommand acts only on the configured v2 root. The server and supervisor
// lock files stay in place so a process starting during prune cannot acquire
// a new inode while the old one is still locked.
func pruneCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("prune", flag.ContinueOnError)
	flags.SetOutput(stderr)
	all := flags.Bool("a", false, "remove conversation data and settings")
	yes := flags.Bool("y", false, "skip confirmation")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aimebu prune [-a] [-y]")
		return 2
	}
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	root, err := safePruneRoot(config.root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !*yes {
		mode := "agent runtime state"
		if *all {
			mode = "all server and supervisor data"
		}
		fmt.Fprintf(stdout, "Remove %s under %s? [y/N] ", mode, root)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(answer)) != "y" {
			fmt.Fprintln(stdout, "Prune cancelled.")
			return 0
		}
	}
	if err := pruneRoot(ctx, root, *all); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "Prune complete.")
	return 0
}

func safePruneRoot(path string) (string, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return "", errors.New("refusing to prune filesystem root")
	}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		parent := filepath.Dir(root)
		resolved, resolveErr := filepath.EvalSymlinks(parent)
		if resolveErr != nil {
			return "", resolveErr
		}
		if resolved != parent {
			return "", fmt.Errorf("config root has a symlinked parent: %s", root)
		}
		return root, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("config root must be a real directory: %s", root)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if resolved != root {
		return "", fmt.Errorf("config root has a symlinked parent: %s", root)
	}
	return root, nil
}

func pruneRoot(ctx context.Context, root string, all bool) error {
	if _, err := safePruneRoot(root); err != nil {
		return err
	}
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		if err := os.Mkdir(root, 0700); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := safePruneRoot(root); err != nil {
		return err
	}
	serverDir := filepath.Join(root, "server")
	supervisorDir := filepath.Join(root, "supervisor")
	for _, dir := range []string{serverDir, supervisorDir} {
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			if err := os.Mkdir(dir, 0700); err != nil {
				return err
			}
			info, err = os.Lstat(dir)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing unsafe prune directory: %s", dir)
		}
	}
	var serverLock, supervisorLock *os.File
	var err error
	serverLock, err = pruneLock(filepath.Join(serverDir, ".writer.lock"), "server")
	if err != nil {
		return err
	}
	defer unlockServerDirectory(serverLock)
	supervisorLock, err = pruneLock(filepath.Join(supervisorDir, "journal.sqlite.lock"), "supervisor")
	if err != nil {
		return err
	}
	defer unlockServerDirectory(supervisorLock)
	if all {
		if err := removeChildrenExcept(serverDir, append([]string{".writer.lock", "backups"}, keptServerLogs()...)...); err != nil {
			return err
		}
		return removeChildrenExcept(supervisorDir, "journal.sqlite.lock")
	}
	dbPath := filepath.Join(serverDir, "aimebu.sqlite")
	if info, err := os.Lstat(dbPath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing unsafe database path: %s", dbPath)
		}
		db, err := store.Open(ctx, dbPath, store.Options{})
		if err != nil {
			return err
		}
		if err := db.PruneRuntime(ctx); err != nil {
			_ = db.Close()
			return err
		}
		if err := db.Close(); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, name := range []string{"journal.sqlite", "journal.sqlite-wal", "journal.sqlite-shm", "agent-tools", "codex", "pi", "opencode", "vibe"} {
		if err := os.RemoveAll(filepath.Join(supervisorDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func pruneLock(path, owner string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing unsafe %s lock: %s", owner, path)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("refusing unsafe %s lock: %s", owner, path)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%s is running; stop it before pruning: %w", owner, err)
	}
	return file, nil
}

func removeChildrenExcept(dir string, keep ...string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		preserved := false
		for _, name := range keep {
			if entry.Name() == name {
				preserved = true
				break
			}
		}
		if preserved {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
