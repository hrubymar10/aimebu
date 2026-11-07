package telemetry

import (
	"context"
	"sync"

	"github.com/getsentry/sentry-go"
)

// UserKey is the root span attribute that carries the id a request's reports name its person by; the processor moves it to the transaction's
// user and never lets it into the span data.
const UserKey = "aimebu.user"

// maxUserID bounds the id a report carries (a login name is chosen by a person).
const maxUserID = 64

// userSlot is filled in once the request has been authenticated, which happens after the middleware has started it.
type userSlot struct {
	mu sync.Mutex
	id string
}

type userSlotKey struct{}

func (u *userSlot) set(id string) { u.mu.Lock(); u.id = id; u.mu.Unlock() }
func (u *userSlot) get() string   { u.mu.Lock(); defer u.mu.Unlock(); return u.id }

// SetUser says who the request in ctx is, by the one id its reports carry: the person's random fingerprint, or their login when they chose
// to send it. Nothing else about them ever goes in. It does nothing outside a request the middleware started.
func SetUser(ctx context.Context, id string) {
	if slot, ok := ctx.Value(userSlotKey{}).(*userSlot); ok {
		slot.set(id)
	}
}

func userFrom(ctx context.Context) string {
	if slot, ok := ctx.Value(userSlotKey{}).(*userSlot); ok {
		return slot.get()
	}
	return ""
}

func trimUserID(id string) string {
	if len(id) > maxUserID {
		id = id[:maxUserID]
	}
	return id
}

// scopeFor is a fresh scope that names the request's person (only the id), for an event captured while serving it.
func scopeFor(ctx context.Context) *sentry.Scope {
	scope := sentry.NewScope()
	if id := userFrom(ctx); id != "" {
		scope.SetUser(sentry.User{ID: trimUserID(id)})
	}
	return scope
}
