package credtarget

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/internal/usages"
)

// ErrLockBusy is returned when the target's write lock could not be taken
// within the timeout; the caller skips the tick and retries later.
var ErrLockBusy = errors.New("credtarget: storage-write lock busy")

// ErrLockLost is returned when the lock could not be kept alive while the file
// was being written (its mtime could not be refreshed). The write completed, but
// the other holders' exclusion was not guaranteed, so the caller treats the target
// as unverified and writes it again on its next pass.
var ErrLockLost = errors.New("credtarget: storage-write lock lost during the write")

// ErrRefreshTokenPresent is returned when the file on disk still holds a refresh
// token: a /login landed in the target and the server has not imported it yet.
// Overwriting it would destroy the only copy of that login, so WriteTarget
// refuses; only a caller that has rescued and imported it may replace it, through
// WriteImportedLogin.
//
// It wraps usages.ErrTargetHoldsLogin, which is how the refresher recognises it
// without importing this package.
var ErrRefreshTokenPresent = fmt.Errorf("credtarget: target holds a refresh token that has not been imported: %w", usages.ErrTargetHoldsLogin)

const (
	defaultLockTimeout       = 3 * time.Second
	defaultHeartbeatInterval = 5 * time.Second
	// storageLockStale is how old a lock directory's mtime may get before it is
	// taken over. It matches Claude's own use of proper-lockfile (stale 15 s,
	// updated every 7.5 s), so a crashed holder is replaced by the same rule.
	storageLockStale = 15 * time.Second
)

// Writer writes targets atomically.
type Writer struct {
	// LockTimeout bounds the wait for <configdir>/.storage-write.lock
	// (claude only). Zero means three seconds.
	LockTimeout time.Duration
	// HeartbeatInterval is how often a held lock's mtime is refreshed so it is
	// never taken for stale. Zero means five seconds, which must stay below half
	// of the stale window.
	HeartbeatInterval time.Duration
	afterLock         func() // test hook: runs while the lock is held
}

// WriteTarget builds and writes t's new content from the full credential. It
// never overwrites a refresh token that has not been imported: if the file on
// disk holds one it returns ErrRefreshTokenPresent and changes nothing, so the
// refresher and Switch can fan out freely without ever destroying a fresh login.
//
// Claude: take `<configdir>/.storage-write.lock` with proper-lockfile
// semantics (a directory created with mkdir, its mtime refreshed while held,
// stale after 15 s, released with rmdir), re-read the file under the lock
// (workers write mcpOAuth into it under the same lock), BuildTarget(existing,
// full), then a temp file in the SAME directory (0600, owner preserved when
// running privileged), fsync, rename. Codex: the same write without the lock
// (codex has no cross-process lock). A missing target is created (file 0600,
// directory 0700 if it had to be made).
//
// The write is skipped when the built content equals what is on disk, so an
// idle tick never touches the file's mtime; the comparison is against the built
// token-only document, so a file that holds a refresh token always differs. A
// symlink at the target path is replaced by a regular file; the file it pointed
// to is not modified.
func (w *Writer) WriteTarget(t Target, full []byte) error {
	return w.write(t, full, false, false)
}

// WriteImportedLogin is WriteTarget for a caller that has already rescued and
// imported the refresh token the file may hold: the reconciler stripping a
// target after a /login import, and migrate-copies after it saved the live
// credential to the store. It is a separate method rather than a flag so that
// nothing shared can be flipped: every other caller keeps the guard.
func (w *Writer) WriteImportedLogin(t Target, full []byte) error {
	return w.write(t, full, true, false)
}

// WriteFullTarget restores a full credential on disable. A pending /login is
// never overwritten; the reconciler must import it first.
func (w *Writer) WriteFullTarget(t Target, full []byte) error {
	return w.write(t, full, false, true)
}

func (w *Writer) write(t Target, full []byte, loginImported, restoreFull bool) (err error) {
	if t.Tool != ToolClaude && t.Tool != ToolCodex {
		return fmt.Errorf("credtarget: unknown tool %q", t.Tool)
	}
	if !filepath.IsAbs(t.Path) {
		return fmt.Errorf("credtarget: target path %q is not absolute", t.Path)
	}
	dir := filepath.Dir(t.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if t.Tool == ToolClaude {
		lock, lockErr := w.lockStorage(dir)
		if lockErr != nil {
			return lockErr
		}
		defer func() {
			if lost := lock.release(); lost && err == nil {
				err = ErrLockLost
			}
		}()
	}
	if w.afterLock != nil {
		w.afterLock()
	}
	existing, err := os.ReadFile(t.Path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !loginImported && refreshTokenOnDisk(t.Tool, existing) {
		return fmt.Errorf("%w: %s", ErrRefreshTokenPresent, t.Path)
	}
	var built []byte
	if restoreFull {
		built, err = BuildFullTarget(t.Tool, existing, full)
	} else {
		built, err = BuildTarget(t.Tool, existing, full)
	}
	if err != nil {
		return err
	}
	info, statErr := os.Stat(t.Path)
	if bytes.Equal(existing, built) && statErr == nil {
		if info.Mode().Perm() != 0o600 {
			return os.Chmod(t.Path, 0o600)
		}
		return nil
	}
	return writeAtomic(t.Path, built, info)
}

// writeAtomic puts data at path through a temp file in the same directory, so a
// reader sees the old file or the new one and never a partial one. info, when
// not nil, describes the file being replaced: its owner is kept when the process
// is privileged enough to set it.
func writeAtomic(path string, data []byte, info os.FileInfo) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if info != nil && os.Geteuid() == 0 {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && (st.Uid != 0 || st.Gid != 0) {
			if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil {
				return err
			}
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // best effort: make the rename itself durable
		_ = d.Close()
	}
	return nil
}

// refreshTokenOnDisk reports whether a credential document holds a usable refresh
// token: claude, a non-empty `refreshToken` (or `refresh_token`) in its OAuth
// section; codex, a non-empty `tokens.refresh_token` (or `refreshToken`).
func refreshTokenOnDisk(tool string, data []byte) bool {
	if len(bytes.TrimSpace(data)) == 0 {
		return false
	}
	doc, err := parseObject(data)
	if err != nil {
		return false
	}
	var sections []string
	var keys []string
	if tool == ToolClaude {
		sections, keys = []string{claudeSection, claudeSectionLegacy}, claudeRefreshKeys
	} else {
		sections, keys = []string{codexTokens}, []string{codexRefreshKey, "refreshToken"}
	}
	for _, name := range sections {
		raw, ok := doc.value(name)
		if !ok {
			continue
		}
		section, err := parseObject(raw)
		if err != nil {
			continue
		}
		for _, key := range keys {
			if value, ok := section.value(key); ok {
				var token string
				if json.Unmarshal(value, &token) == nil && token != "" {
					return true
				}
			}
		}
	}
	return false
}

// StorageWriteLockPath is `<configdir>/.storage-write.lock`.
func StorageWriteLockPath(configDir string) string { return configDir + "/.storage-write.lock" }

// storageLock is a held <configdir>/.storage-write.lock.
type storageLock struct {
	path        string
	stop        chan struct{}
	done        chan struct{}
	compromised atomic.Bool
}

// lockStorage takes Claude's write lock for configDir the way Node's
// proper-lockfile does: the lock is a directory next to the file it protects
// (mkdir is atomic), named after the resolved path, kept alive by refreshing its
// mtime, and taken over once that mtime is older than the stale window.
func (w *Writer) lockStorage(configDir string) (*storageLock, error) {
	real, err := filepath.EvalSymlinks(configDir)
	if err != nil {
		real = configDir
	}
	path := StorageWriteLockPath(real)
	timeout := w.LockTimeout
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		info, statErr := os.Stat(path)
		switch {
		case statErr != nil && errors.Is(statErr, fs.ErrNotExist):
			continue // released between our mkdir and our stat
		case statErr == nil && time.Since(info.ModTime()) > storageLockStale:
			// The holder died or stalled: remove its lock and try again. Only one
			// contender can win the following mkdir.
			if rmErr := os.Remove(path); rmErr == nil || errors.Is(rmErr, fs.ErrNotExist) {
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s", ErrLockBusy, path)
		}
		time.Sleep(time.Duration(10+rand.IntN(40)) * time.Millisecond)
	}
	lock := &storageLock{path: path, stop: make(chan struct{}), done: make(chan struct{})}
	interval := w.HeartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	go lock.heartbeat(interval)
	return lock, nil
}

func (l *storageLock) heartbeat(interval time.Duration) {
	defer close(l.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-ticker.C:
			if err := os.Chtimes(l.path, now, now); err != nil {
				l.compromised.Store(true)
				return
			}
		}
	}
}

// release lets go of the lock and reports whether its heartbeat failed while it
// was held.
func (l *storageLock) release() (lost bool) {
	close(l.stop)
	<-l.done
	_ = os.Remove(l.path)
	return l.compromised.Load()
}
