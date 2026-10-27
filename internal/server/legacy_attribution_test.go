package server

import (
	"context"
	"sync"
	"testing"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

// attributionSupervisor answers the host's attribution scans from a fixed list and counts them.
type attributionSupervisor struct {
	mu    sync.Mutex
	scans []protocol.Frame
}

func (a *attributionSupervisor) scanCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.scans)
}

func serveAttributions(t *testing.T, f legacyFixture, supports bool, attributions []protocol.SessionAttribution) *attributionSupervisor {
	t.Helper()
	fake := &attributionSupervisor{}
	f.srv.machineMu.Lock()
	machine := f.srv.machines[f.machineID]
	machine.supportsSessionScan, machine.supportsSessionAttribution = true, supports
	f.srv.machineMu.Unlock()
	go func() {
		for {
			select {
			case request := <-machine.sendQueue:
				if request.Type != protocol.SessionScan {
					continue
				}
				fake.mu.Lock()
				fake.scans = append(fake.scans, request)
				fake.mu.Unlock()
				reply := protocol.Frame{V: protocol.Version, Type: protocol.SessionScanResult, ReqID: request.ReqID}
				for _, item := range attributions {
					if request.ScanAgent == "" || request.ScanAgent == item.Sender {
						reply.Attributions = append(reply.Attributions, item)
					}
				}
				machine.harnessMu.Lock()
				waiter := machine.pathPending[request.ReqID]
				machine.harnessMu.Unlock()
				if waiter != nil {
					waiter <- reply
				}
			case <-machine.done:
				return
			}
		}
	}()
	return fake
}

func TestBestAttributionPrefersTheSessionHoldingTheLatestMessage(t *testing.T) {
	t.Parallel()
	mine := []int64{300, 250, 200, 100}
	candidates := []protocol.SessionAttribution{
		{ID: "most-of-them", Sender: "s@p", MessageNos: []int64{100, 200, 250}, ModifiedAt: "2026-10-03T10:00:00Z"},
		{ID: "has-the-latest", Sender: "s@p", MessageNos: []int64{300}, ModifiedAt: "2026-10-01T10:00:00Z"},
		{ID: "others-only", Sender: "s@p", MessageNos: []int64{999}, ModifiedAt: "2026-10-04T10:00:00Z"},
	}
	if got := bestAttribution(candidates, mine); got == nil || got.ID != "has-the-latest" {
		t.Fatalf("best = %+v, want the session holding the latest message", got)
	}
	// Without the latest message anywhere, the one with the newest of the sender's messages, then the most of them, wins.
	if got := bestAttribution(candidates[:1], mine); got == nil || got.ID != "most-of-them" {
		t.Fatalf("best of one = %+v", got)
	}
	if got := bestAttribution(candidates[2:], mine); got != nil {
		t.Fatalf("a session that sent none of the sender's messages was offered: %+v", got)
	}
	if bestAttribution(nil, mine) != nil || bestAttribution(candidates, nil) != nil {
		t.Fatal("nothing to match, nothing offered")
	}
}

// For a v1 sender whose messages carry no session, the host's transcripts say which session sent its latest message; that session is
// offered instead of the one v1 kept under the name, the scan is read once for the profile and the start, and an older supervisor leaves
// the store's answer.
func TestResolveLegacyStartOffersTheSessionThatSentTheLatestMessage(t *testing.T) {
	f := newLegacyFixture(t)
	if _, err := f.exec(`UPDATE message SET no=37965 WHERE id='msg_legacy'`); err != nil {
		t.Fatal(err)
	}
	const sender = "0f7c6a1e-aaaa-4222-8333-444455556666"
	fake := serveAttributions(t, f, true, []protocol.SessionAttribution{
		{ID: sender, CWD: f.cwd, Sender: "sage@old", MessageNos: []int64{37900, 37965}, ModifiedAt: "2026-10-02T09:30:00Z"},
		{ID: "0f7c6a1e-bbbb-4222-8333-444455556666", CWD: f.cwd, Sender: "sage@old", MessageNos: []int64{12}, ModifiedAt: "2026-10-05T09:30:00Z"},
	})
	ctx := context.Background()
	got, err := f.srv.resolveLegacyStart(ctx, f.project.ID, "sage@old")
	if err != nil || got.SessionID != sender || !got.Exact || got.Tool != "claude" || got.CWD != f.cwd || got.Mismatch != "" {
		t.Fatalf("resolved = %+v %v, want the session that sent message 37965", got, err)
	}
	scan := fake.scans[0]
	if !scan.ScanAttribution || scan.ScanAgent != "sage@old" || scan.ScanSince == "" || len(scan.ScanFolders) == 0 {
		t.Fatalf("the scan asked for %+v", scan)
	}
	if again, err := f.srv.resolveLegacyStart(ctx, f.project.ID, "sage@old"); err != nil || again.SessionID != sender || fake.scanCount() != 1 {
		t.Fatalf("second resolve: %+v %v after %d scans, want the cached answer", again, err, fake.scanCount())
	}
}

func TestResolveLegacyStartWithoutAttributionKeepsTheStoresAnswer(t *testing.T) {
	f := newLegacyFixture(t)
	if _, err := f.exec(`UPDATE message SET no=37965 WHERE id='msg_legacy'`); err != nil {
		t.Fatal(err)
	}
	fake := serveAttributions(t, f, false, []protocol.SessionAttribution{{ID: "0f7c6a1e-aaaa-4222-8333-444455556666", CWD: f.cwd, Sender: "sage@old", MessageNos: []int64{37965}}})
	got, err := f.srv.resolveLegacyStart(context.Background(), f.project.ID, "sage@old")
	if err != nil || got.SessionID != legacyHandle || got.Exact || fake.scanCount() != 0 {
		t.Fatalf("resolved = %+v %v after %d scans, want the session v1 kept and no scan for an old supervisor", got, err, fake.scanCount())
	}
	// A scan that finds no session holding the sender's messages changes nothing either.
	f2 := newLegacyFixture(t)
	if _, err := f2.exec(`UPDATE message SET no=37965 WHERE id='msg_legacy'`); err != nil {
		t.Fatal(err)
	}
	serveAttributions(t, f2, true, []protocol.SessionAttribution{{ID: "0f7c6a1e-cccc-4222-8333-444455556666", CWD: f2.cwd, Sender: "sage@old", MessageNos: []int64{5}}})
	if got, err := f2.srv.resolveLegacyStart(context.Background(), f2.project.ID, "sage@old"); err != nil || got.SessionID != legacyHandle || got.Exact {
		t.Fatalf("no holder: %+v %v", got, err)
	}
}
