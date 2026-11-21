package server

import (
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
