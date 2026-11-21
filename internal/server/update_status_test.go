package server

import (
	"context"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/updatecheck"
)

// The update-status endpoint: the owner and admins read what the check
// concluded and answer skip or remind; everyone else is refused.

func TestUpdateStatusAnswersAndRecordsAcks(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)

	// Nothing has been checked yet: the answer says so and the acks are refused.
	code, body := w.json(t, w.owner, "GET", "/api/update-status", nil)
	if code != 200 || body["checked"] != false {
		t.Fatalf("unchecked status: code=%d body=%v", code, body)
	}
	if code := w.call(t, w.owner, "POST", "/api/update-status/skip", nil); code != 409 {
		t.Fatalf("skip with no status: %d", code)
	}

	// A check concluded: available for v2.1.0.
	w.f.srv.updateStatus.store(updatecheck.Status{State: "available", Version: "v2.1.0", Channel: "stable", CheckedAt: time.Now().UTC()})

	// The owner and an admin read it; a plain member does not.
	code, body = w.json(t, w.owner, "GET", "/api/update-status", nil)
	if code != 200 || body["state"] != "available" || body["version"] != "v2.1.0" {
		t.Fatalf("owner status: code=%d body=%v", code, body)
	}
	code, body = w.json(t, w.admin, "GET", "/api/update-status", nil)
	if code != 200 || body["state"] != "available" {
		t.Fatalf("admin status: code=%d body=%v", code, body)
	}
	if code := w.call(t, w.viewer, "GET", "/api/update-status", nil); code != 403 {
		t.Fatalf("viewer status: %d", code)
	}

	// A skip is remembered on the account and names the version and state it covers.
	if code := w.call(t, w.admin, "POST", "/api/update-status/skip", nil); code != 204 {
		t.Fatalf("admin skip: %d", code)
	}
	code, body = w.json(t, w.admin, "GET", "/api/update-status", nil)
	ack, _ := body["ack"].(map[string]any)
	if code != 200 || ack["kind"] != "skip" || ack["version"] != "v2.1.0" || ack["state"] != "available" {
		t.Fatalf("skipped ack: code=%d body=%v", code, body)
	}
	// The owner's answer is their own: the owner still has no ack.
	if _, body = w.json(t, w.owner, "GET", "/api/update-status", nil); body["ack"] != nil {
		t.Fatalf("the owner inherited the admin's ack: %v", body)
	}

	// A remind carries a 24-hour deadline.
	if code := w.call(t, w.owner, "POST", "/api/update-status/remind", nil); code != 204 {
		t.Fatalf("owner remind: %d", code)
	}
	ownerAck, hasAck, err := w.f.db.UpdateStatusAck(t.Context(), w.owner.id)
	if err != nil || !hasAck || ownerAck.Kind != "remind" || ownerAck.Until == "" {
		t.Fatalf("owner remind ack: %+v %v", ownerAck, err)
	}
	until, parseErr := time.Parse("2006-01-02T15:04:05.000000000Z", ownerAck.Until)
	if parseErr != nil {
		t.Fatalf("remind until %q does not parse: %v", ownerAck.Until, parseErr)
	}
	if left := time.Until(until); left < 23*time.Hour || left > 25*time.Hour {
		t.Fatalf("remind until is %s away, want about 24h", left)
	}
}

func TestUpdateStatusEventIsASystemEvent(t *testing.T) {
	t.Parallel()
	if !store.IsSystemEventKind("update_status") {
		t.Fatal("update_status is not in the system-events feed")
	}
}

// Skip and remind need the same rights as reading the status.
func TestUpdateStatusAnswersNeedAnAdmin(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	w.f.srv.updateStatus.store(updatecheck.Status{State: "available", Version: "v2.1.0", Channel: "stable", CheckedAt: time.Now().UTC()})
	for _, path := range []string{"/api/update-status/skip", "/api/update-status/remind"} {
		if code := w.call(t, w.viewer, "POST", path, nil); code != 403 {
			t.Fatalf("viewer POST %s: %d, want 403", path, code)
		}
	}
	if _, has, err := w.f.db.UpdateStatusAck(t.Context(), w.viewer.id); err != nil || has {
		t.Fatalf("a refused answer was stored: %v %v", has, err)
	}
	for _, path := range []string{"/api/update-status/skip", "/api/update-status/remind"} {
		if code := w.call(t, w.admin, "POST", path, nil); code != 204 {
			t.Fatalf("admin POST %s: %d, want 204", path, code)
		}
	}
}

// The update_status event reaches every global admin, not only the first user, and
// nobody else; and a later empty check clears the stored status and says so.
func TestUpdateStatusEventsReachEveryAdminAndClear(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	ctx := t.Context()
	_, last, err := w.f.db.EventBounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w.f.srv.recordUpdateStatus(ctx, updatecheck.Status{State: "available", Version: "v2.1.0", Channel: "stable", CheckedAt: time.Now().UTC()})
	seen := func(user string) []store.Event {
		t.Helper()
		_, through, err := w.f.db.EventBounds(ctx)
		if err != nil {
			t.Fatal(err)
		}
		events, err := w.f.db.EventsAfter(ctx, user, last, through)
		if err != nil {
			t.Fatal(err)
		}
		var out []store.Event
		for _, e := range events {
			if e.Kind == "update_status" {
				out = append(out, e)
			}
		}
		return out
	}
	if n := len(seen(w.owner.id)); n != 1 {
		t.Fatalf("owner saw %d update_status events, want 1", n)
	}
	if n := len(seen(w.admin.id)); n != 1 {
		t.Fatalf("admin saw %d update_status events, want 1", n)
	}
	if n := len(seen(w.viewer.id)); n != 0 {
		t.Fatalf("a plain member saw %d update_status events, want 0", n)
	}

	// The same conclusion again is not news; an empty one clears the status and is.
	w.f.srv.recordUpdateStatus(ctx, updatecheck.Status{State: "available", Version: "v2.1.0", CheckedAt: time.Now().UTC()})
	if n := len(seen(w.owner.id)); n != 1 {
		t.Fatalf("a repeated conclusion emitted again: %d events", n)
	}
	w.f.srv.recordUpdateStatus(ctx, updatecheck.Status{CheckedAt: time.Now().UTC()})
	if n := len(seen(w.admin.id)); n != 2 {
		t.Fatalf("clearing emitted %d events for the admin, want 2 in all", n)
	}
	if status, _ := w.f.srv.updateStatus.snapshot(); status.State != "" {
		t.Fatalf("the stored status was not cleared: %+v", status)
	}
	code, body := w.json(t, w.admin, "GET", "/api/update-status", nil)
	if code != 200 || body["state"] != "" {
		t.Fatalf("cleared status answer: code=%d body=%v", code, body)
	}
	// Empty after empty says nothing.
	w.f.srv.recordUpdateStatus(ctx, updatecheck.Status{CheckedAt: time.Now().UTC()})
	if n := len(seen(w.admin.id)); n != 2 {
		t.Fatalf("an empty check after an empty check emitted: %d", n)
	}
}

// A status is stored only after its event committed: a failed event write leaves the
// previous status, and the next check announces the change again.
func TestUpdateStatusIsStoredOnlyAfterItsEventCommits(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	failed, cancel := context.WithCancel(t.Context())
	cancel()
	status := updatecheck.Status{State: "unbuilt", Version: "v2.1.0-3-gabcdef1", CheckedAt: time.Now().UTC()}
	w.f.srv.recordUpdateStatus(failed, status)
	if _, set := w.f.srv.updateStatus.snapshot(); set {
		t.Fatal("a status was stored although its event could not be written")
	}
	w.f.srv.recordUpdateStatus(t.Context(), status)
	if got, set := w.f.srv.updateStatus.snapshot(); !set || got.State != "unbuilt" {
		t.Fatalf("the retry did not store the status: %+v %v", got, set)
	}
}

// An ack may name every state the check reports, the unbuilt checkout included.
func TestUpdateStatusAcksTheUnbuiltState(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	w.f.srv.updateStatus.store(updatecheck.Status{State: "unbuilt", Version: "v2.1.0-3-gabcdef1", CheckedAt: time.Now().UTC()})
	if code := w.call(t, w.owner, "POST", "/api/update-status/skip", nil); code != 204 {
		t.Fatalf("skip of an unbuilt notice: %d", code)
	}
	code, body := w.json(t, w.owner, "GET", "/api/update-status", nil)
	if ack, _ := body["ack"].(map[string]any); code != 200 || ack["state"] != "unbuilt" {
		t.Fatalf("unbuilt ack: code=%d body=%v", code, body)
	}
}
