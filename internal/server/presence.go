package server

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

// How long after a person's last real activity one of their browser sessions
// still counts them as online. A person is online while any of their sessions
// has shown real activity within this window; after it the member card goes
// offline, live, without a reload.
const HumanPresenceWindow = 10 * time.Minute

// How often the presence sweep looks for people whose window has run out and
// tells their projects' members. It bounds how late an offline switch can be
// announced when nobody is reading the member list at the moment it happens.
const humanPresenceSweepInterval = 30 * time.Second

// humanPresence tracks, per person, when a browser session of theirs last
// showed real activity, and announces online/offline transitions as
// `human_presence_changed` events so open member lists refresh without a
// reload. It lives in memory only: after a server restart nobody is online
// until their next activity, which is the honest answer about sessions the
// restarted process knows nothing about.
//
// What counts as real activity, and what does not:
//   - the page's own heartbeat, which the web client sends only while the page
//     is visible and the person has interacted with it (a page visible plus
//     user input); the auth service calls the hook for it like for any other
//     request that changes something;
//   - any authenticated user action, that is any authenticated request that
//     changes something (the auth service's ActivityHook).
//
// Plain background polling (the web client's periodic reads are all GETs) and
// a merely open idle event-stream socket do not count: reads never touch the
// tracker, and the socket's session checks do not either.
type humanPresence struct {
	mu       sync.Mutex
	now      func() time.Time
	active   map[string]time.Time             // a person's last real activity
	stated   map[string]bool                  // the state the last transition announced, so a sweep says a change only once
	announce func(userID string, online bool) // nil until StartHumanPresence wires it; tests replace it
}

func newHumanPresence() *humanPresence {
	return &humanPresence{now: time.Now, active: map[string]time.Time{}, stated: map[string]bool{}}
}

// Touch records real activity by the person. Coming online is announced at
// once; going offline waits for the sweep.
func (p *humanPresence) Touch(userID string) {
	p.mu.Lock()
	online := p.onlineLocked(userID)
	p.active[userID] = p.now()
	if online {
		p.mu.Unlock()
		return
	}
	p.stated[userID] = true
	announce := p.announce
	p.mu.Unlock()
	if announce != nil {
		announce(userID, true)
	}
}

// Online says whether one of the person's sessions showed real activity within
// the window. A nil tracker (tests that build a server without presence) means
// nobody is ever online.
func (p *humanPresence) Online(userID string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.onlineLocked(userID)
}

func (p *humanPresence) onlineLocked(userID string) bool {
	last, ok := p.active[userID]
	if !ok {
		return false
	}
	return p.now().Sub(last) < HumanPresenceWindow
}

// sweep moves everyone whose window ran out to offline and announces it once.
func (p *humanPresence) sweep() {
	p.mu.Lock()
	var wentOffline []string
	for id, online := range p.stated {
		if online && !p.onlineLocked(id) {
			p.stated[id] = false
			wentOffline = append(wentOffline, id)
		}
	}
	announce := p.announce
	p.mu.Unlock()
	if announce == nil {
		return
	}
	for _, id := range wentOffline {
		announce(id, false)
	}
}

// StartHumanPresence wires transition announcements to the event store and
// starts the sweep. Without it the tracker still answers reads; it just never
// announces, so open member lists learn of a switch at their next refresh.
func (s *Server) StartHumanPresence(ctx context.Context) {
	if s.Presence == nil {
		return
	}
	s.Presence.announce = func(userID string, online bool) { s.announcePresence(ctx, userID, online) }
	go func() {
		defer telemetry.Recover(ctx)
		ticker := time.NewTicker(humanPresenceSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Presence.sweep()
			}
		}
	}()
}

// announcePresence records a `human_presence_changed` event in every project
// the person belongs to, so the event stream carries it to that project's
// members and their member lists refresh live. A failure only delays what the
// next refresh shows anyway; it is logged, never raised into a request.
func (s *Server) announcePresence(parent context.Context, userID string, online bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	projects, err := s.Store.UserProjectIDs(ctx, userID)
	if err != nil {
		log.Printf("presence: projects of %s: %v", userID, err)
		telemetry.ReportError(ctx, "presence", err)
		return
	}
	onlineWord := "offline"
	if online {
		onlineWord = "online"
	}
	payload, err := json.Marshal(map[string]string{"user_id": userID, "online": onlineWord})
	if err != nil {
		log.Printf("presence: payload of %s: %v", userID, err)
		return
	}
	for _, projectID := range projects {
		err := s.Store.Tx(ctx, func(tx *store.Tx) error {
			_, err := tx.AppendEvent(store.Event{Kind: "human_presence_changed", ProjectID: projectID, PayloadJSON: string(payload)})
			return err
		})
		if err != nil {
			log.Printf("presence: announce %s %s in %s: %v", userID, onlineWord, projectID, err)
			telemetry.ReportError(ctx, "presence", err)
		}
	}
}
