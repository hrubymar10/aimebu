package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Every authenticated request used to commit a session-expiry write, and each
// commit costs an fsync behind the store's single connection. Parallel calls
// with a fresh cookie must not write.
func TestParallelAuthenticatedRequestsDoNotWriteTheSession(t *testing.T) {
	f := newFixture(t)
	sum := sha256.Sum256([]byte(f.cookie.Value))
	hash := hex.EncodeToString(sum[:])
	before, err := f.db.SessionByTokenHash(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Auth.Now = func() time.Time { return time.Now().Add(time.Minute) }
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := f.request(http.MethodGet, "/api/me", nil, false)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("status=%d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	after, err := f.db.SessionByTokenHash(context.Background(), hash)
	if err != nil || after.ExpiresAt != before.ExpiresAt {
		t.Fatalf("session written by reads: %s -> %s (%v)", before.ExpiresAt, after.ExpiresAt, err)
	}
}
