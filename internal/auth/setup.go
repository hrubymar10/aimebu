package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	ErrSetupClosed       = errors.New("setup is closed")
	ErrSetupToken        = errors.New("invalid setup token")
	ErrSetupCredentials  = errors.New("invalid owner credentials")
	setupMinPasswordSize = 12
)

// OwnerSetup is the one-time first-run owner setup. The server mints a random token when it starts without an owner,
// prints it (and a link that carries it) and keeps a copy in a 0600 file; whoever holds the token may create the owner once.
// Only the token's hash stays in memory. The token is never served over the API.
type OwnerSetup struct {
	// Create makes the owner and the host machine in one transaction (the same code as the environment bootstrap).
	Create   func(ctx context.Context, login, password string) (ownerID string, err error)
	Throttle *Throttler
	Now      func() time.Time

	mu   sync.Mutex
	hash [sha256.Size]byte
	path string
	open bool
}

// NewOwnerSetup mints a fresh token, writes it to path (replacing the file of an earlier start, so a restart
// invalidates the old link) and returns it for the startup message.
func NewOwnerSetup(path string, create func(ctx context.Context, login, password string) (string, error)) (*OwnerSetup, string, error) {
	token, err := randomToken("setup_")
	if err != nil {
		return nil, "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0600); err != nil {
		return nil, "", err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		_ = os.Remove(tmp)
		return nil, "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, "", err
	}
	return &OwnerSetup{Create: create, Throttle: NewThrottler(), Now: time.Now, hash: sha256.Sum256([]byte(token)), path: path, open: true}, token, nil
}

// Open reports whether a setup token is still waiting to be used.
func (setup *OwnerSetup) Open() bool {
	if setup == nil {
		return false
	}
	setup.mu.Lock()
	defer setup.mu.Unlock()
	return setup.open
}

// Close drops the token (an owner appeared some other way) and removes its file.
func (setup *OwnerSetup) Close() {
	if setup == nil {
		return
	}
	setup.mu.Lock()
	defer setup.mu.Unlock()
	setup.closeLocked()
}

func (setup *OwnerSetup) closeLocked() {
	setup.open = false
	setup.hash = [sha256.Size]byte{}
	_ = os.Remove(setup.path)
}

// Path is the file that holds the token (what the setup screen tells the person to look in).
func (setup *OwnerSetup) Path() string {
	if setup == nil {
		return ""
	}
	return setup.path
}

// Verify checks a token the same way Redeem does (constant time, a wrong guess counts against the caller's IP) but creates nothing:
// the setup screen uses it to say at once whether the token is the right one before it asks for the owner's login.
func (setup *OwnerSetup) Verify(token, remoteIP string) error {
	if setup == nil {
		return ErrSetupClosed
	}
	now := setup.Now().UTC()
	if !setup.Throttle.reserve(setupThrottleKey(remoteIP), remoteIP, now) {
		return ErrRateLimited
	}
	defer setup.Throttle.release(setupThrottleKey(remoteIP), remoteIP)
	setup.mu.Lock()
	defer setup.mu.Unlock()
	if !setup.open {
		return ErrSetupClosed
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	if subtle.ConstantTimeCompare(got[:], setup.hash[:]) != 1 {
		setup.Throttle.fail(setupThrottleKey(remoteIP), remoteIP, setup.Now().UTC())
		return ErrSetupToken
	}
	return nil
}

// RetryAfter is how long a caller that was refused with ErrRateLimited should wait.
func (setup *OwnerSetup) RetryAfter(remoteIP string) time.Duration {
	return setup.Throttle.RetryAfter(setupThrottleKey(remoteIP), remoteIP, setup.Now().UTC())
}

// Redeem checks the token in constant time and creates the owner. A wrong token counts against the caller's IP and
// the setup as a whole; bad credentials do not burn a correct token, so a typo does not lock the person out.
// The token is spent only when the owner exists. Attempts are serialized, so two concurrent submissions of the
// same token cannot both create an owner.
func (setup *OwnerSetup) Redeem(ctx context.Context, token, login, password, remoteIP string) (string, error) {
	if setup == nil {
		return "", ErrSetupClosed
	}
	now := setup.Now().UTC()
	if !setup.Throttle.reserve(setupThrottleKey(remoteIP), remoteIP, now) {
		return "", ErrRateLimited
	}
	defer setup.Throttle.release(setupThrottleKey(remoteIP), remoteIP)
	setup.mu.Lock()
	defer setup.mu.Unlock()
	if !setup.open {
		return "", ErrSetupClosed
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	if subtle.ConstantTimeCompare(got[:], setup.hash[:]) != 1 {
		setup.Throttle.fail(setupThrottleKey(remoteIP), remoteIP, setup.Now().UTC())
		return "", ErrSetupToken
	}
	if ValidateOwnerCredentials(login, password) != nil {
		return "", ErrSetupCredentials
	}
	login = strings.ToLower(strings.TrimSpace(login))
	ownerID, err := setup.Create(ctx, login, password)
	if err != nil {
		return "", err
	}
	setup.closeLocked()
	setup.Throttle.reset(setupThrottleKey(remoteIP), remoteIP)
	return ownerID, nil
}

// ValidateOwnerCredentials is the rule for the first owner, whether created in the browser or from the environment: the login is
// 3-32 lowercase letters, digits, - or _ starting with a letter (capitals and outer spaces are normalized away) and the password
// has at least 12 characters, the same as for invitations.
func ValidateOwnerCredentials(login, password string) error {
	if !validLogin(strings.ToLower(strings.TrimSpace(login))) {
		return errors.New("the owner login must be 3-32 lowercase letters, digits, - or _, starting with a letter")
	}
	if len(password) < setupMinPasswordSize {
		return errors.New("the owner password must have at least 12 characters")
	}
	return nil
}

// setupThrottleKey keeps one source's wrong guesses from limiting anyone else: the throttler's login key is made per IP, so it never
// forms a global lock a stranger could use to keep the owner out. The 256-bit token makes guessing hopeless anyway; the limit only
// bounds noise.
func setupThrottleKey(remoteIP string) string { return "owner-setup:" + remoteIP }
