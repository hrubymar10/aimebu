package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/store"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrRateLimited        = errors.New("login temporarily limited")
	ErrInvalidSession     = errors.New("invalid session")
	ErrInvalidKey         = errors.New("invalid key")
	ErrBootstrapRequired  = errors.New("set AIMEBU2_OWNER_LOGIN and AIMEBU2_OWNER_PASSWORD to initialize the owner")
)

const sessionTimeFormat = "2006-01-02T15:04:05.000000000Z"

type Service struct {
	Store      *store.Store
	Policy     PasswordPolicy
	SessionTTL time.Duration
	// MaxSessionAge is the absolute lifetime of a session, counted from when it was made: the sliding expiry never carries it past this, so a
	// stolen session token stops working after it however steadily it is used.
	MaxSessionAge time.Duration
	Throttle      *Throttler
	PairThrottle  *Throttler
	Now           func() time.Time
	// PublicURL is the HTTP(S) origin browsers use when a reverse proxy
	// fronts the server (AIMEBU2_PUBLIC_URL); empty when unset.
	PublicURL string
}

func NewService(db *store.Store) *Service {
	return &Service{Store: db, Policy: DefaultPasswordPolicy,
		SessionTTL: 7 * 24 * time.Hour, MaxSessionAge: 30 * 24 * time.Hour, Throttle: NewLoginThrottler(), PairThrottle: NewThrottler(), Now: time.Now}
}

type LoginResult struct {
	UserID       string
	SessionToken string
	CSRFToken    string
	ExpiresAt    time.Time
}

func (service *Service) Login(ctx context.Context, login, password, remoteIP string) (LoginResult, error) {
	submittedLogin := login
	login = strings.ToLower(strings.TrimSpace(login))
	now := service.Now().UTC()
	if !service.Throttle.reserve(login, remoteIP, now) {
		return LoginResult{}, ErrRateLimited
	}
	defer service.Throttle.release(login, remoteIP)
	// Guesses that cannot be right are refused before any password work: an empty password or name and a name no login is as long as are not worth
	// a 600,000-iteration hash, and the refusal counts against the source like any other wrong guess.
	if password == "" || login == "" || len(login) > 256 {
		service.Throttle.fail(login, remoteIP, service.Now().UTC())
		return LoginResult{}, ErrInvalidCredentials
	}
	maxIterations, err := service.Store.MaxPasswordIterations(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	if maxIterations < service.Policy.Iterations {
		maxIterations = service.Policy.Iterations
	}
	if maxIterations > 10_000_000 {
		maxIterations = 10_000_000
	}
	user, err := service.Store.UserByLogin(ctx, submittedLogin)
	if errors.Is(err, sql.ErrNoRows) {
		dummyPasswordWork(password, PasswordPolicy{Iterations: maxIterations, SaltBytes: service.Policy.SaltBytes})
		service.Throttle.fail(login, remoteIP, service.Now().UTC())
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, err
	}
	matched, rehash, err := verifyPasswordAtCost(password, user.PWHash, user.PWParams, service.Policy, maxIterations)
	if err != nil || !matched || user.DisabledAt.Valid {
		service.Throttle.fail(login, remoteIP, service.Now().UTC())
		return LoginResult{}, ErrInvalidCredentials
	}
	service.Throttle.reset(login, remoteIP)
	if rehash {
		hash, params, err := HashPassword(password, service.Policy)
		if err != nil {
			return LoginResult{}, err
		}
		if err := service.Store.UpdateUserPassword(ctx, user.ID, hash, params); err != nil {
			return LoginResult{}, err
		}
	}
	return service.StartSession(ctx, user.ID)
}

// StartSession opens a session for a user whose identity the caller has already established.
func (service *Service) StartSession(ctx context.Context, userID string) (LoginResult, error) {
	now := service.Now().UTC()
	token, err := randomToken("as_")
	if err != nil {
		return LoginResult{}, err
	}
	csrf, err := randomToken("csrf_")
	if err != nil {
		return LoginResult{}, err
	}
	expires := now.Add(service.SessionTTL)
	_, err = service.Store.CreateAuthSession(ctx, store.AuthSession{UserID: userID,
		TokenHash: hashToken(token), CSRFToken: csrf,
		ExpiresAt: expires.Format(sessionTimeFormat), CreatedAt: now.Format(sessionTimeFormat)})
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{userID, token, csrf, expires}, nil
}

// Authenticate checks a bearer token and slides a valid session's expiry.
func (service *Service) Authenticate(ctx context.Context, token string) (store.AuthSession, error) {
	session, err := service.CheckSession(ctx, token)
	if err != nil {
		return store.AuthSession{}, err
	}
	now := service.Now().UTC()
	newExpiry := now.Add(service.SessionTTL)
	// The expiry slides, but never past the session's absolute lifetime.
	if created, ok := service.sessionCreated(session); ok && service.MaxSessionAge > 0 {
		if limit := created.Add(service.MaxSessionAge); newExpiry.After(limit) {
			newExpiry = limit
		}
	}
	// Every commit costs an fsync and the store has one connection, so a
	// write per request queues a page load's parallel calls behind each
	// other. Only slide the expiry once it lags noticeably.
	if stored, err := time.Parse(sessionTimeFormat, session.ExpiresAt); err == nil &&
		newExpiry.Sub(stored) < service.slideThreshold() {
		return session, nil
	}
	if err := service.Store.ExtendAuthSession(ctx, session.ID, now, newExpiry); err != nil {
		return store.AuthSession{}, err
	}
	session.ExpiresAt = newExpiry.Format(sessionTimeFormat)
	return session, nil
}

// slideThreshold is how far the stored expiry may lag the would-be new one
// before Authenticate writes: a tenth of the TTL, at least five minutes, and
// never more than half the TTL so short TTLs still slide.
func (service *Service) slideThreshold() time.Duration {
	threshold := service.SessionTTL / 10
	if threshold < 5*time.Minute {
		threshold = 5 * time.Minute
	}
	if limit := service.SessionTTL / 2; threshold > limit {
		threshold = limit
	}
	return threshold
}

// CheckSession validates a bearer token without renewing its expiry. Long-lived
// sockets use this so an idle connection does not keep a session alive.
func (service *Service) CheckSession(ctx context.Context, token string) (store.AuthSession, error) {
	if !strings.HasPrefix(token, "as_") || len(token) < 40 {
		return store.AuthSession{}, ErrInvalidSession
	}
	session, err := service.Store.SessionByTokenHash(ctx, hashToken(token))
	if errors.Is(err, sql.ErrNoRows) {
		return store.AuthSession{}, ErrInvalidSession
	}
	if err != nil {
		return store.AuthSession{}, err
	}
	if session.RevokedAt.Valid {
		return store.AuthSession{}, ErrInvalidSession
	}
	now := service.Now().UTC()
	expires, err := time.Parse(sessionTimeFormat, session.ExpiresAt)
	if err != nil || !expires.After(now) {
		return store.AuthSession{}, ErrInvalidSession
	}
	if service.MaxSessionAge > 0 {
		// A session whose age cannot be read is not trusted: its lifetime would be unbounded.
		created, ok := service.sessionCreated(session)
		if !ok || !created.Add(service.MaxSessionAge).After(now) {
			return store.AuthSession{}, ErrInvalidSession
		}
	}
	return session, nil
}

func (service *Service) sessionCreated(session store.AuthSession) (time.Time, bool) {
	created, err := time.Parse(time.RFC3339Nano, session.CreatedAt)
	return created.UTC(), err == nil
}

func (service *Service) RevokeSession(ctx context.Context, token string) error {
	session, err := service.Store.SessionByTokenHash(ctx, hashToken(token))
	if err != nil {
		return ErrInvalidSession
	}
	return service.Store.RevokeAuthSession(ctx, session.ID)
}

// Bootstrap creates the sole initial owner and approved host machine together.
// The host key is returned once; only its hash is retained in SQLite.
func (service *Service) Bootstrap(ctx context.Context, login, password, hostName string) (
	ownerID, machineID, machineKey string, created bool, err error) {
	return service.BootstrapWithKeySink(ctx, login, password, hostName, nil)
}

// BootstrapWithKeySink writes the only copy of the host key before the owner
// transaction commits. A failed write leaves no owner or machine behind.
func (service *Service) BootstrapWithKeySink(ctx context.Context, login, password, hostName string,
	keySink func(machineID, machineKey string) error) (
	ownerID, machineID, machineKey string, created bool, err error) {
	login = strings.ToLower(strings.TrimSpace(login))
	count, err := service.Store.UserCount(ctx)
	if err != nil || count != 0 {
		return "", "", "", false, err
	}
	if strings.TrimSpace(login) == "" || password == "" {
		return "", "", "", false, ErrBootstrapRequired
	}
	hash, params, err := HashPassword(password, service.Policy)
	if err != nil {
		return "", "", "", false, err
	}
	machineKey, err = randomToken("amk_")
	if err != nil {
		return "", "", "", false, err
	}
	err = service.Store.Tx(ctx, func(tx *store.Tx) error {
		count, err := tx.CountUsers()
		if err != nil {
			return err
		}
		if count != 0 {
			created = false
			return nil
		}
		user, err := tx.CreateUser(store.User{Login: login, PWHash: hash, PWParams: params})
		if err != nil {
			return err
		}
		machine, err := tx.CreateMachine(store.Machine{Name: hostName,
			KeyHash: hashToken(machineKey), Approved: true, IsHost: true})
		if err != nil {
			return err
		}
		ownerID, machineID, created = user.ID, machine.ID, true
		if keySink != nil {
			return keySink(machineID, machineKey)
		}
		return nil
	})
	if err != nil || !created {
		return "", "", "", false, err
	}
	return ownerID, machineID, machineKey, true, nil
}

func (service *Service) IssueAgentKey(ctx context.Context, agentID string) (string, error) {
	key, err := randomToken("aak_")
	if err != nil {
		return "", err
	}
	if err := service.Store.SetAgentKeyHash(ctx, agentID, hashToken(key)); err != nil {
		return "", err
	}
	return key, nil
}

func (service *Service) VerifyAgentKey(ctx context.Context, agentID, key string) error {
	hash, revoked, err := service.Store.AgentKeyByID(ctx, agentID)
	if err != nil || revoked || !verifyKeyHash(hash, key, "aak_") {
		return ErrInvalidKey
	}
	return nil
}

func (service *Service) IssueMachineKey(ctx context.Context, machineID string) (string, error) {
	key, err := randomToken("amk_")
	if err != nil {
		return "", err
	}
	if err := service.Store.SetMachineKeyHash(ctx, machineID, hashToken(key)); err != nil {
		return "", err
	}
	return key, nil
}

const PairingTTL = 10 * time.Minute

func (service *Service) CreateMachinePairing(ctx context.Context, name, userID string) (string, time.Time, error) {
	if strings.TrimSpace(name) == "" || len(name) > 100 {
		return "", time.Time{}, ErrInvalidCredentials
	}
	token, err := randomToken("amp_")
	if err != nil {
		return "", time.Time{}, err
	}
	expires := service.Now().UTC().Add(PairingTTL)
	err = service.Store.CreateMachinePairing(ctx, hashToken(token), strings.TrimSpace(name), userID, expires.Format(sessionTimeFormat))
	return token, expires, err
}

// pairingThrottleKey keys pairing backoff by the submitted token, like user
// invitations, so guesses against one token cannot delay every other pairing.
func pairingThrottleKey(token string) string { return "machine-pairing:" + hashToken(token) }

// PairingRetryAfter reports the backoff for a token and client IP.
func (service *Service) PairingRetryAfter(token, remoteIP string) time.Duration {
	return service.PairThrottle.RetryAfter(pairingThrottleKey(token), remoteIP, service.Now())
}

// LoginRetryAfter is how long a login attempt from this source must wait.
func (service *Service) LoginRetryAfter(login, remoteIP string) time.Duration {
	return service.Throttle.RetryAfter(strings.ToLower(strings.TrimSpace(login)), remoteIP, service.Now())
}

func (service *Service) RedeemMachinePairing(ctx context.Context, token, remoteIP string) (store.Machine, string, error) {
	now := service.Now().UTC()
	throttleName := pairingThrottleKey(token)
	if !service.PairThrottle.reserve(throttleName, remoteIP, now) {
		return store.Machine{}, "", ErrRateLimited
	}
	defer service.PairThrottle.release(throttleName, remoteIP)
	if !strings.HasPrefix(token, "amp_") || len(token) < 40 {
		service.PairThrottle.fail(throttleName, remoteIP, service.Now().UTC())
		return store.Machine{}, "", ErrInvalidCredentials
	}
	key, err := randomToken("amk_")
	if err != nil {
		return store.Machine{}, "", err
	}
	machine, err := service.Store.RedeemMachinePairing(ctx, hashToken(token), hashToken(key), now.Format(sessionTimeFormat))
	if errors.Is(err, sql.ErrNoRows) {
		service.PairThrottle.fail(throttleName, remoteIP, service.Now().UTC())
		return store.Machine{}, "", ErrInvalidCredentials
	}
	if err != nil {
		return store.Machine{}, "", err
	}
	service.PairThrottle.reset(throttleName, remoteIP)
	return machine, key, nil
}

// MachinePairingStatus accepts a pending key without granting socket access.
func (service *Service) MachinePairingStatus(ctx context.Context, id, key string) (store.Machine, error) {
	m, err := service.Store.MachineByID(ctx, id)
	if err != nil || m.RevokedAt.Valid || !verifyKeyHash(m.KeyHash, key, "amk_") {
		return store.Machine{}, ErrInvalidKey
	}
	return m, nil
}

func (service *Service) VerifyMachineKey(ctx context.Context, machineID, key string) error {
	_, err := service.VerifiedMachineKeyHash(ctx, machineID, key)
	return err
}

// VerifiedMachineKeyHash authenticates the bearer once and returns only its
// stored digest for long-lived socket state.
func (service *Service) VerifiedMachineKeyHash(ctx context.Context, machineID, key string) (string, error) {
	machine, err := service.Store.MachineByID(ctx, machineID)
	if err != nil || !machine.Approved || machine.RevokedAt.Valid ||
		!verifyKeyHash(machine.KeyHash, key, "amk_") {
		return "", ErrInvalidKey
	}
	return machine.KeyHash, nil
}

func (service *Service) VerifyMachineKeyHash(ctx context.Context, machineID, expectedHash string) error {
	machine, err := service.Store.MachineByID(ctx, machineID)
	if err != nil || !machine.Approved || machine.RevokedAt.Valid ||
		subtle.ConstantTimeCompare([]byte(machine.KeyHash), []byte(expectedHash)) != 1 {
		return ErrInvalidKey
	}
	return nil
}

func randomToken(prefix string) (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func verifyKeyHash(storedHash, key, prefix string) bool {
	if len(key) != len(prefix)+43 { // 32 bytes in unpadded base64url.
		return false
	}
	want, err := hex.DecodeString(storedHash)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256([]byte(key))
	prefixMatch := subtle.ConstantTimeCompare([]byte(key[:len(prefix)]), []byte(prefix))
	hashMatch := subtle.ConstantTimeCompare(got[:], want)
	return prefixMatch&hashMatch == 1
}
