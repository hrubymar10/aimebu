package store

import (
	"context"
	"strings"
	"testing"
)

func TestArchiveSearchScopesRoomsAndEscapesWildcards(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	project, err := s.CreateProject(ctx, Project{Name: "archive", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateProject(ctx, Project{Name: "other", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	makeRoom := func(projectID, kind, title string) Room {
		t.Helper()
		key := ""
		if kind == "dm" {
			key = title
		}
		r, err := s.CreateRoom(ctx, Room{ProjectID: projectID, Kind: kind, Title: title, CreatedBy: "usr_peer", DMKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	archived := makeRoom(project.ID, "task", "Archived work")
	open := makeRoom(project.ID, "task", "Open work")
	dm := makeRoom(project.ID, "dm", "Private")
	foreign := makeRoom(other.ID, "task", "Other project")
	for _, user := range []string{"usr_peer", "usr_other"} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?,'hash','{}','now')`, user, user); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddMember(ctx, dm.ID, "user", "usr_peer"); err != nil {
		t.Fatal(err)
	}
	for _, room := range []Room{archived, dm, foreign} {
		if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, room.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		room Room
		body string
	}{
		{archived, "literal 100%_ match"}, {open, "literal 100%_ match"},
		{dm, "literal 100%_ secret"}, {foreign, "literal 100%_ remote"},
	} {
		if _, err := s.InsertMessage(ctx, Message{RoomID: entry.room.ID, AuthorKind: "system", AuthorID: "system", Body: entry.body}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.ArchiveSearch(ctx, project.ID, "usr_other", "100%_", 50)
	if err != nil || len(hits) != 1 || hits[0].RoomID != archived.ID || hits[0].RoomName != archived.Title || !strings.Contains(hits[0].Snippet, "100%_") {
		t.Fatalf("literal search: %+v %v", hits, err)
	}
	hits, err = s.ArchiveSearch(ctx, project.ID, "usr_peer", "100%_", 50)
	if err != nil || len(hits) != 2 {
		t.Fatalf("peer results: %+v %v", hits, err)
	}
	for i := 0; i < 55; i++ {
		if _, err := s.InsertMessage(ctx, Message{RoomID: archived.ID, AuthorKind: "system", AuthorID: "system", Body: "needle"}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err = s.ArchiveSearch(ctx, project.ID, "usr_other", "needle", 100)
	if err != nil || len(hits) != 50 {
		t.Fatalf("limit: %d %v", len(hits), err)
	}
}

func TestArchiveSearchSnippetIsRuneSafe(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("č", 100) + "needle" + strings.Repeat("ž", 100)
	snippet := snippetAround(body, "needle")
	if !strings.Contains(snippet, "needle") || !strings.HasPrefix(snippet, "…") || !strings.HasSuffix(snippet, "…") || len([]rune(snippet)) > 130 {
		t.Fatalf("snippet %q", snippet)
	}
}

func TestArchiveSearchRespectsRestrictedRoomAccess(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: f.other.ID, AuthorKind: "system", AuthorID: "system", Body: "private routing needle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE room SET lifecycle='closed',restricted=1 WHERE id=?`, f.other.ID); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{f.alice, f.carol} {
		hits, err := f.s.ArchiveSearch(ctx, f.project.ID, user, "routing needle", 50)
		if err != nil || len(hits) != 1 || hits[0].RoomID != f.other.ID {
			t.Fatalf("oversight archive for %s: %+v %v", user, hits, err)
		}
	}
	hits, err := f.s.ArchiveSearch(ctx, f.project.ID, f.bob, "routing needle", 50)
	if err != nil || len(hits) != 0 {
		t.Fatalf("viewer searched restricted archive: %+v %v", hits, err)
	}
	if err := f.s.AddMember(ctx, f.other.ID, "user", f.bob); err != nil {
		t.Fatal(err)
	}
	hits, err = f.s.ArchiveSearch(ctx, f.project.ID, f.bob, "routing needle", 50)
	if err != nil || len(hits) != 1 || hits[0].RoomID != f.other.ID {
		t.Fatalf("room member archive: %+v %v", hits, err)
	}
}
