package usages

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// ReactiveOutcome is what a reactive refresh did.
type ReactiveOutcome string

const (
	// ReactiveRotated: a new credential was committed and fanned out; the rejected state can be re-read.
	ReactiveRotated ReactiveOutcome = "rotated"
	// ReactiveRevoked: the provider refused the refresh token (invalid_grant and kin); the session is gone.
	ReactiveRevoked ReactiveOutcome = "revoked"
	// ReactiveFailed: the refresh did not complete for another reason; the credential is untouched.
	ReactiveFailed ReactiveOutcome = "failed"
	// ReactiveSkipped: nothing was spent (not owned, not active, already tried for this token, busy, or a guard held).
	ReactiveSkipped ReactiveOutcome = "skipped"
)

// ReactiveResult is the outcome of RefreshRejected with a one-line, token-free detail.
type ReactiveResult struct {
	Outcome ReactiveOutcome
	Detail  string
}

// RefreshRejected is the reactive refresh: the account rejected the ACTIVE profile's login (a usage poll answered 401/403), and the
// expiry-driven due check cannot see that, because a token the provider ended early still carries a far exp. It treats the profile
// as due once and spends through the same path as a scheduled refresh (the account lock, the journal, the commit and the fan-out).
//
// Limits: one attempt per access-token hash for the life of the process, so a token that stays rejected never spends again; a
// profile that is not the tool's active one, or a tool the refresher does not own, is left alone. A failed refresh keeps every
// credential as it was. A revoked session is announced by the spend itself ("the session was revoked"); any other failure that
// raised no alert of its own is announced here ("the refresh failed: <class>").
func (r *TargetRefresher) RefreshRejected(ctx context.Context, tool, profile string) ReactiveResult {
	skip := func(why string) ReactiveResult { return ReactiveResult{Outcome: ReactiveSkipped, Detail: why} }
	if !r.ownsTool(tool) {
		return skip("the refresher does not own " + tool)
	}
	profiles, err := r.rawProfiles(tool)
	if err != nil {
		return skip("cannot list the profiles")
	}
	var p StoredProfile
	found := false
	for _, candidate := range profiles {
		if candidate.Name == profile {
			p, found = candidate, true
			break
		}
	}
	if !found || !p.Active || !p.HasCredentials || p.CredPath == "" {
		return skip("not the active profile of " + tool)
	}
	data, err := os.ReadFile(p.CredPath)
	if err != nil {
		return skip("the stored credential cannot be read")
	}
	access := accessTokenOf(tool, data)
	if access == "" {
		return skip("the stored credential has no access token")
	}
	hash := tokenHash(tool, access)
	key := tool + "/" + p.Name
	r.mu.Lock()
	stoppedAt, stopped := r.reactiveStopped[key]
	rotatedTo := r.reactiveRotated[key]
	if stopped && stoppedAt != hash {
		// The profile holds a different credential than the one rejected after a rotation: a new login was imported (or the token
		// changed some other way). The stop, its alert and the gap start over.
		r.clearReactiveLocked(key)
		stopped, rotatedTo = false, ""
	}
	r.mu.Unlock()
	if stopped {
		return skip("stopped: the account rejects freshly refreshed tokens; log in again")
	}
	if rotatedTo != "" && rotatedTo == hash {
		// A reactive rotation succeeded and the token it produced is rejected as well: more spends would only burn refresh tokens
		// on a dead account. Stop until a new login is imported or the person switches profiles; one alert says so.
		r.mu.Lock()
		if r.reactiveStopped == nil {
			r.reactiveStopped = map[string]string{}
		}
		r.reactiveStopped[key] = hash
		delete(r.reactiveRotated, key)
		r.mu.Unlock()
		r.emit("reactive_refresh_stopped", key, "the account rejects freshly refreshed tokens; log in again")
		return skip("stopped: the account rejects freshly refreshed tokens; log in again")
	}
	r.mu.Lock()
	tried := r.reactiveTried[hash]
	last, hasLast := r.reactiveLast[key]
	r.mu.Unlock()
	if tried {
		return skip("this access token was already refreshed once after a rejection")
	}
	// A provider that rejects every token it issues (a suspended account) would otherwise turn each rotation into the next
	// attempt. One reactive spend per profile in a minimum gap bounds that; the next poll after the gap tries again.
	if hasLast && r.now().Sub(last) < refreshMinGap {
		return skip("a reactive refresh of " + key + " ran less than " + refreshMinGap.String() + " ago")
	}
	// The token is only marked once the run got past every guard (a busy account lock, a pending login, a backoff or the
	// migration quiet period leave it eligible for the next poll).
	attempted := func() {
		r.mu.Lock()
		if r.reactiveTried == nil {
			r.reactiveTried = map[string]bool{}
		}
		if r.reactiveLast == nil {
			r.reactiveLast = map[string]time.Time{}
		}
		r.reactiveTried[hash] = true
		r.reactiveLast[key] = r.now()
		r.mu.Unlock()
		log.Printf("usages: token refresh %s: the login was rejected, refreshing it once (token %s)", key, hash[:8])
	}
	err = r.refreshProfileOpts(ctx, p, false, true, attempted)
	after, _ := os.ReadFile(p.CredPath)
	switch {
	case err == nil && accessTokenOf(tool, after) != access:
		r.mu.Lock()
		if r.reactiveRotated == nil {
			r.reactiveRotated = map[string]string{}
		}
		r.reactiveRotated[key] = tokenHash(tool, accessTokenOf(tool, after))
		r.mu.Unlock()
		log.Printf("usages: token refresh %s: the rejected login was refreshed (outcome=rotated)", key)
		return ReactiveResult{Outcome: ReactiveRotated, Detail: "rotated"}
	case err == nil:
		// Nothing was sent: a guard held (busy lock, pending login, backoff). Not an account failure.
		return skip("the refresh did not run; see the skip reason in the log")
	case errors.Is(err, ErrAccountNeedsLogin):
		// The spend (or the state it found) already raised the one alert, which names revoked or unknown.
		state := r.AccountStates()[key]
		if state == AccountReauthRequired {
			return ReactiveResult{Outcome: ReactiveRevoked, Detail: "the session was revoked"}
		}
		return ReactiveResult{Outcome: ReactiveFailed, Detail: "the refresh failed: " + state}
	case errors.Is(err, ErrRefreshBackoff):
		return skip("the account is backing off after an earlier failure")
	default:
		detail := fmt.Sprintf("the refresh failed: %s", cleanDetail(err.Error()))
		if !accountReported(err) {
			r.emit("reactive_refresh_failed", key, detail)
		}
		return ReactiveResult{Outcome: ReactiveFailed, Detail: detail}
	}
}

// clearReactiveLocked forgets everything the reactive refresh remembers about key: the rotation it made, the stop, the gap since its last
// spend and the once-per-process alert, so a later rejection starts from a clean slate. Callers hold r.mu.
func (r *TargetRefresher) clearReactiveLocked(key string) {
	delete(r.reactiveRotated, key)
	delete(r.reactiveStopped, key)
	delete(r.reactiveLast, key)
	delete(r.alerted, "reactive_refresh_stopped/"+key)
}

// ClearReactiveStop ends the stop (and the gap) for every profile of tool: a login was imported, or the person switched profiles, so
// the next rejection is a new situation. The server calls it from the switch and import paths; an import the reconciler makes is seen
// by RefreshRejected itself, because the profile then holds a different access token.
func (r *TargetRefresher) ClearReactiveStop(tool string) {
	prefix := tool + "/"
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range []map[string]string{r.reactiveRotated, r.reactiveStopped} {
		for key := range m {
			if strings.HasPrefix(key, prefix) {
				r.clearReactiveLocked(key)
			}
		}
	}
	for key := range r.reactiveLast {
		if strings.HasPrefix(key, prefix) {
			r.clearReactiveLocked(key)
		}
	}
}
