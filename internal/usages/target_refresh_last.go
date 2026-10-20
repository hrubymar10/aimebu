package usages

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

// Safety net against a rotation loop. The refresh lead decides when a profile is
// due; if it is longer than a token's life (the acceptance knob allows that on
// purpose, once), a freshly rotated token is due again on the very next tick and
// every tick would spend a refresh token. So the refresher remembers when each
// profile last rotated and how long its token lives, and
//
//   - never refreshes a profile again within refreshMinGap of its last rotation;
//   - caps the lead at that token life minus refreshMinGap, so a profile that was
//     just rotated is not due until it is within half an hour of expiring.
//
// The record is advisory state in switcher/refresh-last.json (0600, no
// credentials): losing it only loses the cap for the first rotation after a
// restart, and the floor still applies from then on.
const (
	// refreshMinGap is the minimum time between two successful rotations of one
	// profile, and the margin kept below the token life when capping the lead.
	refreshMinGap = 30 * time.Minute

	refreshLastFile = "refresh-last.json"
)

type lastRotation struct {
	At        time.Time `json:"at"`
	ExpiresAt time.Time `json:"expires_at"`
	// TokenHash is the hash of the refresh token the rotation produced. The floor
	// and the lead cap hold only while the profile still stores that token: a
	// re-login or import (a different token) is never held back by an earlier
	// rotation.
	TokenHash string `json:"token_hash,omitempty"`
}

// Life is how long the token produced by that rotation lives.
func (l lastRotation) Life() time.Duration { return l.ExpiresAt.Sub(l.At) }

type refreshLastStore struct {
	path string
	mu   sync.Mutex
	data map[string]lastRotation // "tool/profile" -> record
}

func newRefreshLastStore(root string) *refreshLastStore {
	s := &refreshLastStore{data: map[string]lastRotation{}}
	if root == "" {
		return s
	}
	s.path = filepath.Join(root, refreshLastFile)
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	var file struct {
		Version int                     `json:"version"`
		Last    map[string]lastRotation `json:"last"`
	}
	if json.Unmarshal(raw, &file) == nil && file.Version == 1 && file.Last != nil {
		s.data = file.Last
	}
	return s
}

func (s *refreshLastStore) get(tool, name string) (lastRotation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.data[tool+"/"+name]
	return l, ok
}

func (s *refreshLastStore) set(tool, name string, l lastRotation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[tool+"/"+name] = l
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(struct {
		Version int                     `json:"version"`
		Last    map[string]lastRotation `json:"last"`
	}{1, s.data}, "", "  ")
	if err == nil {
		_ = atomicWriteBytes(s.path, append(raw, '\n'), 0o600)
	}
}

func (r *TargetRefresher) lastStore() *refreshLastStore {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = newRefreshLastStore(r.root())
	}
	return r.last
}

// noteRotation records that tool/name just rotated into the credentials in
// rotated.
func (r *TargetRefresher) noteRotation(tool, name string, rotated []byte) {
	expiresAt := tolerantExpiry(tool, rotated)
	token, err := refreshToken(rotated, tool)
	if expiresAt == nil || err != nil {
		return
	}
	r.lastStore().set(tool, name, lastRotation{At: r.now().UTC(), ExpiresAt: expiresAt.UTC(), TokenHash: tokenHash(tool, token)})
}

// storedTokenHash is the hash of the refresh token a profile's store file holds
// now ("" when it cannot be read).
func storedTokenHash(tool, path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	token, err := refreshToken(data, tool)
	if err != nil {
		return ""
	}
	return tokenHash(tool, token)
}

// effectiveLead is the lead to judge tool/name by, and whether the profile is
// inside the minimum gap after its last rotation (in which case it is not due).
// current is the hash of the refresh token the profile stores now; the record of
// an earlier rotation applies only while it is that same token.
func (r *TargetRefresher) effectiveLead(tool, name, current string, now time.Time) (lead time.Duration, tooSoon bool) {
	lead = r.lead[tool]
	last, ok := r.lastStore().get(tool, name)
	if !ok || last.TokenHash != current {
		return lead, false
	}
	if since := now.Sub(last.At); since >= 0 && since < refreshMinGap {
		return lead, true
	}
	if life := last.Life(); life > refreshMinGap && lead > life-refreshMinGap {
		lead = life - refreshMinGap
	}
	return lead, false
}
