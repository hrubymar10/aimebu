package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hrubymar10/aimebu/internal/store"
)

func newSetupFixture(t *testing.T) (*Service, *OwnerSetup, string, string) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "setup.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewService(db)
	service.Policy = PasswordPolicy{Iterations: 1000, SaltBytes: 16}
	path := filepath.Join(t.TempDir(), "setup-token")
	setup, token, err := NewOwnerSetup(path, func(ctx context.Context, login, password string) (string, error) {
		ownerID, _, _, created, err := service.Bootstrap(ctx, login, password, "host")
		if err == nil && !created {
			return "", ErrSetupClosed
		}
		return ownerID, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, setup, token, path
}

func TestSetupTokenFileIsPrivateAndNotTheOnlyCopyInMemory(t *testing.T) {
	_, setup, token, path := newSetupFixture(t)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token file: %v %v", info, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != token || !strings.HasPrefix(token, "setup_") || !setup.Open() {
		t.Fatalf("file=%q token=%q", raw, token)
	}
}

func TestSetupTokenIsSingleUse(t *testing.T) {
	service, setup, token, path := newSetupFixture(t)
	ownerID, err := setup.Redeem(t.Context(), token, "Owner", testPassword, "127.0.0.1")
	if err != nil || ownerID == "" {
		t.Fatalf("redeem: %q %v", ownerID, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the token file survives its use: %v", err)
	}
	if _, err := setup.Redeem(t.Context(), token, "second", testPassword, "127.0.0.1"); !errors.Is(err, ErrSetupClosed) || setup.Open() {
		t.Fatalf("second redeem: %v", err)
	}
	if n, _ := service.Store.UserCount(t.Context()); n != 1 {
		t.Fatalf("users=%d", n)
	}
	if _, err := service.Login(t.Context(), "owner", testPassword, "127.0.0.1"); err != nil {
		t.Fatalf("the owner cannot sign in: %v", err)
	}
}

func TestSetupRejectsBadTokensAndThrottles(t *testing.T) {
	service, setup, token, _ := newSetupFixture(t)
	for i := 0; i <= freeFailureBudget; i++ {
		if _, err := setup.Redeem(t.Context(), "setup_wrong", "owner", testPassword, "10.0.0.1"); !errors.Is(err, ErrSetupToken) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// The failure budget is spent: even the right token waits from that source.
	if _, err := setup.Redeem(t.Context(), token, "owner", testPassword, "10.0.0.1"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after the budget: %v", err)
	}
	if n, _ := service.Store.UserCount(t.Context()); n != 0 {
		t.Fatalf("a throttled attempt created an owner: %d", n)
	}
	// One source's wrong guesses must not lock the owner out from their own address.
	if _, err := setup.Redeem(t.Context(), token, "owner", testPassword, "10.0.0.2"); err != nil {
		t.Fatalf("another address was limited by a stranger's guesses: %v", err)
	}
}

func TestSetupBadCredentialsDoNotBurnTheToken(t *testing.T) {
	_, setup, token, _ := newSetupFixture(t)
	for _, c := range [][2]string{{"", testPassword}, {"owner", "short"}, {"Bad Login!", testPassword}} {
		if _, err := setup.Redeem(t.Context(), token, c[0], c[1], "127.0.0.1"); !errors.Is(err, ErrSetupCredentials) {
			t.Fatalf("%v: %v", c, err)
		}
	}
	if _, err := setup.Redeem(t.Context(), token, "owner", testPassword, "127.0.0.1"); err != nil {
		t.Fatalf("the token was burned by typos: %v", err)
	}
}

func TestSetupConcurrentRedeemCreatesOneOwner(t *testing.T) {
	service, setup, token, _ := newSetupFixture(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := setup.Redeem(t.Context(), token, "owner", testPassword, "127.0.0."+string(rune('1'+i))); err == nil {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if n, _ := service.Store.UserCount(t.Context()); wins.Load() != 1 || n != 1 {
		t.Fatalf("wins=%d users=%d", wins.Load(), n)
	}
}

func TestSetupRestartInvalidatesTheOldToken(t *testing.T) {
	_, _, first, path := newSetupFixture(t)
	second, token2, err := NewOwnerSetup(path, nil)
	if err != nil || token2 == first {
		t.Fatalf("restart: %v", err)
	}
	if _, err := second.Redeem(t.Context(), first, "owner", testPassword, "127.0.0.1"); !errors.Is(err, ErrSetupToken) {
		t.Fatalf("old token: %v", err)
	}
	if raw, _ := os.ReadFile(path); strings.TrimSpace(string(raw)) != token2 {
		t.Fatalf("file not replaced: %q", raw)
	}
}

func TestNilSetupIsClosed(t *testing.T) {
	var setup *OwnerSetup
	if setup.Open() {
		t.Fatal("nil setup is open")
	}
	if _, err := setup.Redeem(t.Context(), "x", "owner", testPassword, "127.0.0.1"); !errors.Is(err, ErrSetupClosed) {
		t.Fatalf("%v", err)
	}
}

func TestSetupVerifyChecksTheTokenWithoutSpendingIt(t *testing.T) {
	created := 0
	setup, token, err := NewOwnerSetup(filepath.Join(t.TempDir(), ".setup_token"), func(context.Context, string, string) (string, error) { created++; return "usr_owner", nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := setup.Path(); filepath.Base(got) != ".setup_token" {
		t.Fatalf("path = %q", got)
	}
	if err := setup.Verify(token, "10.0.0.1"); err != nil {
		t.Fatalf("right token: %v", err)
	}
	if err := setup.Verify(token+"\n", "10.0.0.1"); err != nil {
		t.Fatalf("a pasted token with its newline: %v", err)
	}
	if err := setup.Verify("setup_wrong", "10.0.0.1"); !errors.Is(err, ErrSetupToken) {
		t.Fatalf("wrong token: %v", err)
	}
	if created != 0 || !setup.Open() {
		t.Fatalf("checking a token must create and spend nothing: created=%d open=%t", created, setup.Open())
	}
	if _, err := setup.Redeem(context.Background(), token, "owner", "correct horse battery staple", "10.0.0.1"); err != nil {
		t.Fatalf("the token still works after being checked: %v", err)
	}
	if err := setup.Verify(token, "10.0.0.1"); !errors.Is(err, ErrSetupClosed) {
		t.Fatalf("after setup: %v", err)
	}
}

func TestSetupVerifyCountsWrongGuessesPerIPAndLeavesOthersAlone(t *testing.T) {
	setup, token, err := NewOwnerSetup(filepath.Join(t.TempDir(), ".setup_token"), func(context.Context, string, string) (string, error) { return "usr_owner", nil })
	if err != nil {
		t.Fatal(err)
	}
	limited := false
	for i := 0; i < 20; i++ {
		if err := setup.Verify("setup_wrong", "10.0.0.9"); errors.Is(err, ErrRateLimited) {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("repeated wrong guesses from one address must be rate limited")
	}
	if err := setup.Verify(token, "10.0.0.9"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a limited address stays limited, even with the right token: %v", err)
	}
	if err := setup.Verify(token, "10.0.0.2"); err != nil {
		t.Fatalf("another address must not be locked out by the first: %v", err)
	}
	if setup.RetryAfter("10.0.0.9") <= 0 {
		t.Fatal("a limited address needs a retry-after")
	}
}

func TestNilSetupHasNoPathAndVerifiesNothing(t *testing.T) {
	var setup *OwnerSetup
	if setup.Path() != "" || !errors.Is(setup.Verify("x", "10.0.0.1"), ErrSetupClosed) {
		t.Fatal("a nil setup is closed")
	}
}
