package server

import (
	"os"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// A12-21: reading a room's worktree checks that its directory exists. A directory removed outside the API (git worktree remove) shows as
// removed, is stored once with one event, and a worktree that is still there stays ready.
func TestRoomWorktreeReadShowsMissingDirectoryAsRemoved(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	initRepo(t, f.project.Folder)
	machine, _ := f.db.MachineByID(t.Context(), f.machineID)
	item, err := f.srv.ensureRoomWorktree(t.Context(), f.room, machine, "")
	if err != nil || item.Path == "" {
		t.Fatalf("ensure: %+v %v", item, err)
	}
	read := func() store.RoomWorktree {
		t.Helper()
		resp := f.request("GET", "/api/rooms/"+f.room.ID+"/worktree", nil, false)
		defer resp.Body.Close()
		var got store.RoomWorktree
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&got) != nil {
			t.Fatalf("read: %d", resp.StatusCode)
		}
		return got
	}
	_, before, _ := f.db.EventBounds(t.Context())
	if got := read(); got.State != "ready" {
		t.Fatalf("an existing directory is not ready: %+v", got)
	}
	if _, after, _ := f.db.EventBounds(t.Context()); after != before {
		t.Fatal("reading an existing worktree wrote an event")
	}
	if err := os.RemoveAll(item.Path); err != nil {
		t.Fatal(err)
	}
	got := read()
	if got.State != "removed" || got.Reason == "" {
		t.Fatalf("a missing directory is not removed: %+v", got)
	}
	stored, _, _ := f.db.RoomWorktree(t.Context(), f.room.ID, f.machineID)
	if stored.State != "removed" {
		t.Fatalf("not stored: %+v", stored)
	}
	_, written, _ := f.db.EventBounds(t.Context())
	if written != before+1 {
		t.Fatalf("expected one event: before %d after %d", before, written)
	}
	if got := read(); got.State != "removed" {
		t.Fatalf("second read: %+v", got)
	}
	if _, again, _ := f.db.EventBounds(t.Context()); again != written {
		t.Fatal("a second read wrote again")
	}
}
