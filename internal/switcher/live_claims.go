package switcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// liveClaimSuffix names the claim file that sits next to a live credential file. The claim is an flock held for the life
// of the process that refreshes that file's token, so two refreshers (two roots, an old checkout, a stray dev server) that
// resolve to the same live file cannot both own it, wherever their config directories are. The kernel drops the lock when
// the holder dies, so there is no staleness to guess at. It is a separate file because the live file itself is replaced
// by rename on every refresh, which would drop a lock held on its inode.
const liveClaimSuffix = ".aimebu-refresher.lock"

// LiveClaims holds this process's refresher claim on each live credential file it owns, and reports the tools whose live
// file another refresher holds. Refresh is idempotent and cheap: call it on every tick so a claim lost to a dead holder is
// taken over, and a claim file removed or replaced under a live holder is re-established.
type LiveClaims struct {
	mgr  *Manager
	mu   sync.Mutex
	held map[string]*os.File
	// closed is set by Close at shutdown: from then on nothing re-acquires a claim, so a late caller (the reconciler,
	// a settings poll) cannot take back what shutdown just gave up.
	closed bool
}

func NewLiveClaims(mgr *Manager) *LiveClaims {
	return &LiveClaims{mgr: mgr, held: map[string]*os.File{}}
}

// Refresh re-checks every tool's claim (the timer's job: report a late conflict, take over from a dead holder).
func (c *LiveClaims) Refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tool := range Tools() {
		c.checkLocked(tool)
	}
}

// Blocked is why this process may not refresh tool's live file, or "" when it holds the claim. It acquires the claim
// itself when the tool has just entered the token-copy model, so there is no window between a migration and the next
// timer pass in which the refresher touches the login unclaimed: the answer is "" only for a claim actually held.
// A tool outside the token-copy model is not this process's to refresh and is not blocked.
func (c *LiveClaims) Blocked(tool string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkLocked(tool)
}

func (c *LiveClaims) checkLocked(tool string) string {
	if !c.mgr.TokenCopiesFor(tool) {
		c.releaseLocked(tool)
		return ""
	}
	return c.claimLocked(tool)
}

// Close drops every claim for good: Blocked answers "shutting down" afterwards instead of re-acquiring.
func (c *LiveClaims) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for tool := range c.held {
		c.releaseLocked(tool)
	}
}

// Release drops every claim (tests).
func (c *LiveClaims) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for tool := range c.held {
		c.releaseLocked(tool)
	}
}

func (c *LiveClaims) releaseLocked(tool string) {
	if f := c.held[tool]; f != nil {
		f.Close()
		delete(c.held, tool)
	}
}

// claimLocked returns "" when this process holds the claim, otherwise the sentence for the owner.
func (c *LiveClaims) claimLocked(tool string) string {
	if c.closed {
		return "the server is shutting down"
	}
	live, err := c.mgr.LiveCredPath(tool)
	if err != nil {
		return "could not locate the live credential file: " + err.Error()
	}
	// The harness may never have run here (or its directory was removed): create the directory the claim sits next to
	// rather than treating it as a conflict, which would stop every stored profile of the tool from refreshing and let
	// their tokens expire. 0700 is what the harness itself uses for it.
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		return "could not create the live credential directory " + filepath.Dir(live) + ": " + err.Error()
	}
	if live, err = canonicalTokenPath(live); err != nil {
		return "could not resolve the live credential file: " + err.Error()
	}
	path := live + liveClaimSuffix
	if f := c.held[tool]; f != nil {
		if held, err := f.Stat(); err == nil {
			if now, err := os.Stat(path); err == nil && os.SameFile(held, now) {
				return ""
			}
		}
		c.releaseLocked(tool) // the claim file was removed or replaced: claim again on the new file
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if os.IsNotExist(err) {
		return "the live credential directory " + filepath.Dir(live) + " does not exist, so the refresher claim cannot be taken"
	}
	if err != nil {
		return "could not create the refresher claim next to the live credential file: " + err.Error()
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readClaimHolder(f)
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return "another token refresher already owns " + live + holder + "; stop it so only one refreshes this login"
		}
		return "could not take the refresher claim: " + err.Error()
	}
	f.Truncate(0)
	fmt.Fprintf(f, "root=%s pid=%d\n", c.mgr.Root(), os.Getpid())
	c.held[tool] = f
	return ""
}

// LiveClaimPath is the claim file for tool's live credential file.
func (m *Manager) LiveClaimPath(tool string) (string, error) {
	live, err := m.LiveCredPath(tool)
	if err != nil {
		return "", err
	}
	if live, err = canonicalTokenPath(live); err != nil {
		return "", err
	}
	return live + liveClaimSuffix, nil
}

func readClaimHolder(f *os.File) string {
	buf := make([]byte, 512)
	n, _ := f.ReadAt(buf, 0)
	text := strings.TrimSpace(string(buf[:n]))
	if text == "" {
		return ""
	}
	return " (" + text + ")"
}
