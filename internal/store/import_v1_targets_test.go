package store

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// addV1Rooms puts rooms and one message per entry into the fixture's v1 database. Message numbers are the map's values, so a test
// controls how rooms interleave.
func addV1Rooms(t *testing.T, source string, rooms []string, messages map[int64]string) {
	t.Helper()
	db := openV1Fixture(t, source)
	at := "2026-10-01T12:00:00Z"
	for _, id := range rooms {
		raw, _ := json.Marshal(map[string]any{"id": id, "members": []string{"alice"}, "created_at": at, "created_by": "alice"})
		if _, err := db.Exec(`INSERT INTO rooms(id,data) VALUES (?,?)`, id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for number, room := range messages {
		raw, _ := json.Marshal(map[string]any{"id": number, "room_id": room, "from": "alice", "from_kind": "human", "body": "m" + room, "created_at": at})
		if _, err := db.Exec(`INSERT INTO messages(id,room_id,data) VALUES (?,?,?)`, number, room, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
}

func targetMapping(t *testing.T, root string, base V1ImportMap) V1ImportMap {
	t.Helper()
	m := base
	m.Projects = map[string]V1ImportProject{"repo": base.Projects["repo"], "proj": {Name: "proj", Folder: root + "/proj"}}
	m.Rooms = map[string]V1ImportRoom{
		"work":              {Project: "repo", Lifecycle: "open"},
		"proj-leader":       {Project: "proj", Target: V1TargetRoot},
		"proj":              {Project: "proj", Target: V1TargetSub, Title: "general", Lifecycle: "open"},
		"extra":             {Project: "proj", Target: V1TargetSub, Title: "general"},
		"dm:alice:bot@repo": {Project: "repo", Skip: true},
	}
	m.MemoryProjects = map[string]string{"repo": "repo"}
	return m
}

func TestV1PreviewSuggestsRootAndGeneralForLeaderPairs(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	addV1Rooms(t, source, []string{"proj", "proj-leader", "solo"}, map[int64]string{10: "proj", 11: "proj-leader", 12: "solo"})
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	rooms := preview.Suggested.Rooms
	if got := rooms["proj-leader"]; got.Target != V1TargetRoot || got.Project != "proj" {
		t.Errorf("x-leader goes to the project root: %+v", got)
	}
	if got := rooms["proj"]; got.Target != V1TargetSub || got.Title != "general" || got.Project != "proj" {
		t.Errorf("x goes to a sub-room called general: %+v", got)
	}
	for _, id := range []string{"solo", "work"} {
		if rooms[id].Target != V1TargetRoot || rooms[id].Project != id {
			t.Errorf("%s is neither half of a pair, so it is the root of a project of its own: %+v", id, rooms[id])
		}
	}
	if rooms["_system"].Target != "" {
		t.Errorf("a skipped room has no target: %+v", rooms["_system"])
	}
}

func TestV1ImportPlacesRoomsInTheRootAndSharedSubRooms(t *testing.T) {
	t.Parallel()
	source, base := v1Fixture(t)
	addV1Rooms(t, source, []string{"proj", "proj-leader", "extra"}, map[int64]string{10: "proj-leader", 11: "proj", 12: "extra", 13: "proj-leader"})
	s := newV1RoleStore(t)
	ctx := context.Background()
	mapping := targetMapping(t, t.TempDir(), base)

	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !reflect.DeepEqual(dry.Imported, report.Imported) || !reflect.DeepEqual(dry.TargetWarnings, report.TargetWarnings) {
		t.Errorf("the dry run and the import disagree: %+v %v vs %+v %v", dry.Imported, dry.TargetWarnings, report.Imported, report.TargetWarnings)
	}
	if len(report.TargetWarnings) != 1 || !strings.Contains(report.TargetWarnings[0], "2 v1 rooms are merged into general under ⌂ proj") || !strings.Contains(report.TargetWarnings[0], "extra") || !strings.Contains(report.TargetWarnings[0], "proj") {
		t.Errorf("the shared sub-room is a warning: %v", report.TargetWarnings)
	}

	var homeID string
	if err := s.db.QueryRow(`SELECT r.id FROM room r JOIN project p ON p.id=r.project_id WHERE p.name='proj' AND r.kind='home'`).Scan(&homeID); err != nil {
		t.Fatal(err)
	}
	roomOf := func(number int64) (id, title string) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT m.room_id,r.title FROM message m JOIN room r ON r.id=m.room_id WHERE m.seq=?`, number).Scan(&id, &title); err != nil {
			t.Fatalf("message %d: %v", number, err)
		}
		return
	}
	for _, number := range []int64{10, 13} {
		if id, _ := roomOf(number); id != homeID {
			t.Errorf("message %d should be in the project root, got room %s", number, id)
		}
	}
	generalID, title := roomOf(11)
	if title != "general" || generalID == homeID {
		t.Fatalf("message 11 should be in a sub-room called general: %s %q", generalID, title)
	}
	if id, _ := roomOf(12); id != generalID {
		t.Errorf("v1 'extra' shares the general sub-room, got %s", id)
	}
	var subRooms int
	var parent, lifecycle, kind string
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room WHERE title='general' AND project_id=(SELECT project_id FROM room WHERE id=?)`, homeID).Scan(&subRooms); err != nil || subRooms != 1 {
		t.Errorf("exactly one general room: %d %v", subRooms, err)
	}
	if err := s.db.QueryRow(`SELECT parent_id,lifecycle,kind FROM room WHERE id=?`, generalID).Scan(&parent, &lifecycle, &kind); err != nil || parent != homeID || lifecycle != "open" || kind != "task" {
		t.Errorf("the sub-room sits under the home room and is open because one of its rooms asked: %s %s %s %v", parent, lifecycle, kind, err)
	}
	// Numbers and times are the v1 ones.
	var seq int64
	var created string
	if err := s.db.QueryRow(`SELECT seq,created_at FROM message WHERE seq=10`).Scan(&seq, &created); err != nil || !strings.HasPrefix(created, "2026-10-01T12:00:00") {
		t.Errorf("message 10 keeps its number and time: %d %q %v", seq, created, err)
	}
	// No room is made for the root target; the ledger records each v1 room against where it landed.
	for room, want := range map[string]string{"proj-leader": homeID, "proj": generalID, "extra": generalID} {
		var got string
		if err := s.db.QueryRow(`SELECT new_id FROM legacy_import_map WHERE source_key=? AND entity_kind='room_in' AND old_id=?`, mapping.SourceKey, room).Scan(&got); err != nil || got != want+"|"+room {
			t.Errorf("ledger for %s: %q want %q (%v)", room, got, want+"|"+room, err)
		}
	}

	// A rerun adds nothing: no new room, no new message, every v1 room already present.
	var rooms, messages int
	s.db.QueryRow(`SELECT COUNT(*) FROM room`).Scan(&rooms)
	s.db.QueryRow(`SELECT COUNT(*) FROM message`).Scan(&messages)
	again, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	var rooms2, messages2 int
	s.db.QueryRow(`SELECT COUNT(*) FROM room`).Scan(&rooms2)
	s.db.QueryRow(`SELECT COUNT(*) FROM message`).Scan(&messages2)
	if rooms2 != rooms || messages2 != messages || again.Imported["rooms"] != 0 || again.AlreadyPresent["rooms"] < 4 || len(again.TargetWarnings) != 0 {
		t.Errorf("rerun changed things: rooms %d->%d messages %d->%d report %+v", rooms, rooms2, messages, messages2, again)
	}
}

// A v1 read position carries to the room its messages landed in, judged among that v1 room's own messages.
func TestV1ImportReadPositionsFollowTheTarget(t *testing.T) {
	t.Parallel()
	source, base := v1Fixture(t)
	addV1Rooms(t, source, []string{"proj", "proj-leader", "extra"}, map[int64]string{10: "proj-leader", 11: "proj", 12: "extra", 13: "proj-leader", 14: "proj"})
	db := openV1Fixture(t, source)
	raw, _ := json.Marshal(map[string]any{"id": "alice", "kind": "human", "name": "alice", "read_cursors": map[string]int64{"proj-leader": 12, "proj": 11, "extra": 12}})
	if _, err := db.Exec(`UPDATE agents SET data=? WHERE id='alice'`, string(raw)); err != nil {
		t.Fatal(err)
	}
	s := newV1RoleStore(t)
	ctx := context.Background()
	if _, err := s.ImportV1(ctx, source, targetMapping(t, t.TempDir(), base), false); err != nil {
		t.Fatal(err)
	}
	cursor := func(title string) int64 {
		t.Helper()
		var n int64
		if err := s.db.QueryRow(`SELECT COALESCE((SELECT last_read_seq FROM room_read_cursor c WHERE c.user_id='usr_owner' AND c.room_id=r.id),-1) FROM room r WHERE r.title=?`, title).Scan(&n); err != nil {
			t.Fatalf("%s: %v", title, err)
		}
		return n
	}
	// proj-leader read up to 12 means its own message 10 (13 is unread); the root holds 10 and 13, so the position is 10.
	if got := cursor("proj"); got != 10 {
		t.Errorf("the project root: read through 10, got %d", got)
	}
	// Two v1 rooms share general: proj read 11, extra read 12 (its message is 12); the larger wins.
	if got := cursor("general"); got != 12 {
		t.Errorf("general: the larger position of the rooms merged into it, got %d", got)
	}
}

func TestV1ImportRefusesBadTargets(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		room string
		spec V1ImportRoom
		want string
	}{
		"unknown":       {"work", V1ImportRoom{Project: "repo", Target: "attic"}, `unknown target "attic"`},
		"dm to root":    {"dm:alice:bot@repo", V1ImportRoom{Project: "repo", Target: V1TargetRoot}, "cannot be placed in a project room"},
		"title length":  {"work", V1ImportRoom{Project: "repo", Target: V1TargetSub, Title: strings.Repeat("t", 81)}, "at most 80 characters"},
		"title newline": {"work", V1ImportRoom{Project: "repo", Target: V1TargetSub, Title: "a\nb"}, "one line"},
	} {
		source, mapping := v1Fixture(t)
		s := newV1RoleStore(t)
		mapping.Rooms[c.room] = c.spec
		if _, err := s.ImportV1(context.Background(), source, mapping, true); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, c.want, err)
		}
	}
	if validateV1RoomTarget("x", V1ImportRoom{Target: V1TargetSub}) != nil || v1SubRoomTitle(V1ImportRoom{Title: "  "}) != "general" {
		t.Error("an empty sub-room title means general")
	}
}

// With no default project, a room that has no project is not imported, and the report lists it; so does a memory scope with none.
func TestV1ImportDropsWhatHasNoProject(t *testing.T) {
	t.Parallel()
	source, base := v1Fixture(t)
	addV1Rooms(t, source, []string{"stray", "proj", "proj-leader", "extra"}, map[int64]string{10: "stray", 11: "proj", 12: "proj-leader"})
	s := newV1RoleStore(t)
	ctx := context.Background()
	mapping := targetMapping(t, t.TempDir(), base)
	mapping.DefaultProject = ""
	mapping.Rooms["stray"] = V1ImportRoom{} // no project, not skipped by choice
	delete(mapping.Rooms, "work")           // never mentioned either
	mapping.MemoryProjects = map[string]string{"repo": ""}
	before := mapping.Rooms["stray"]
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatalf("a missing project must not be an error: %v", err)
	}
	if got := strings.Join(report.SkippedNoProject, ","); got != "stray,work" {
		t.Errorf("the rooms dropped for want of a project: %q", got)
	}
	var strays, memory int
	s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE seq IN (10)`).Scan(&strays)
	s.db.QueryRow(`SELECT COUNT(*) FROM project_memory`).Scan(&memory)
	if strays != 0 || memory != 0 {
		t.Errorf("nothing of a dropped room or memory scope is imported: messages=%d memory=%d", strays, memory)
	}
	var kept int
	s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE seq IN (11,12)`).Scan(&kept)
	if kept != 2 {
		t.Errorf("the rooms that do have a project are imported: %d", kept)
	}
	if mapping.Rooms["stray"] != before || mapping.MemoryProjects["repo"] != "" {
		t.Error("the caller's mapping must not be changed")
	}
	// No projects at all is allowed too: everything is dropped, the rest of the backup (macros here) still imports.
	source2, base2 := v1Fixture(t)
	s2 := newV1RoleStore(t)
	empty := base2
	empty.Projects, empty.Rooms, empty.MemoryProjects, empty.DefaultProject = map[string]V1ImportProject{}, map[string]V1ImportRoom{}, map[string]string{}, ""
	if rep, err := s2.ImportV1(ctx, source2, empty, false); err != nil || len(rep.SkippedNoProject) == 0 || rep.Imported["macros"] == 0 {
		t.Errorf("an import with no projects: %v %+v", err, rep.Imported)
	}
}

// The preview invents no catch-all project: pairs and memory scopes become projects, other rooms and every DM start as "don't import".
func TestV1PreviewSuggestsNoDefaultProject(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	addV1Rooms(t, source, []string{"proj", "proj-leader", "solo", "repo", "general", "quiet"}, map[int64]string{10: "proj", 11: "proj-leader", 12: "solo", 13: "repo", 14: "general"})
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Suggested.DefaultProject != "" {
		t.Errorf("no default project: %q", preview.Suggested.DefaultProject)
	}
	if _, ok := preview.Suggested.Projects["imported-v1"]; ok {
		t.Error("no Imported v1 project")
	}
	if _, ok := preview.Suggested.Projects["proj"]; !ok {
		t.Error("a pair is a project")
	}
	if got := preview.Suggested.MemoryProjects["repo"]; got != "repo" || preview.Suggested.Projects["repo"].Name != "repo" {
		t.Errorf("a memory scope with no project is suggested as one: %q %+v", got, preview.Suggested.Projects["repo"])
	}
	rooms := preview.Suggested.Rooms
	// A room with traffic and no leader pair is a project of its own, even though nothing else names it.
	if rooms["solo"].Skip || rooms["solo"].Project != "solo" || rooms["solo"].Target != V1TargetRoot || preview.Suggested.Projects["solo"].Name != "solo" {
		t.Errorf("a room with traffic and no pair is its own project: %+v", rooms["solo"])
	}
	// v1's default room and an empty room are not projects, so they have nowhere to go and start as don't import.
	for _, id := range []string{"general", "quiet"} {
		if !rooms[id].Skip || rooms[id].Project != "" {
			t.Errorf("%s has nowhere to go and starts as don't import: %+v", id, rooms[id])
		}
	}
	if rooms["repo"].Skip || rooms["repo"].Project != "repo" || rooms["repo"].Target != V1TargetRoot || rooms["repo"].Title != "" {
		t.Errorf("a room named like a project with no leader room is the project's root: %+v", rooms["repo"])
	}
	if got := rooms["dm:alice:bot@repo"]; got.Skip || got.Project != "repo" {
		t.Errorf("a DM with a project to go to is imported: %+v", got)
	}
	if rooms["proj-leader"].Target != V1TargetRoot || rooms["proj"].Target != V1TargetSub {
		t.Errorf("with both x-leader and x, the leader is the root and x is general: %+v %+v", rooms["proj-leader"], rooms["proj"])
	}
}

// A macro the owner chose not to bring is not imported, is counted, and is not recorded, so a later run can still bring it.
func TestV1ImportSkipsChosenMacros(t *testing.T) {
	t.Parallel()
	source, base := v1Fixture(t)
	addV1Rooms(t, source, []string{"proj", "proj-leader", "extra"}, map[int64]string{10: "proj", 11: "proj-leader", 12: "extra"})
	s := newV1RoleStore(t)
	ctx := context.Background()
	mapping := targetMapping(t, t.TempDir(), base)
	mapping.SkipMacros = []string{"greet"}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatal(err)
	}
	macros, err := s.ListMacros(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := macros["greet"]; kept || macros["name"] == "" || report.Skipped["macros"] != 1 {
		t.Errorf("greet is skipped and name is imported: %v skipped=%d", macros, report.Skipped["macros"])
	}
	var ledger int
	s.db.QueryRow(`SELECT COUNT(*) FROM legacy_import_map WHERE source_key=? AND entity_kind='macro' AND old_id='greet'`, mapping.SourceKey).Scan(&ledger)
	if ledger != 0 {
		t.Error("a skipped macro is not recorded in the ledger")
	}
}

// If any v1 room merged into a target had memory off, the target has memory off, for the project root and a shared sub-room alike and
// whichever room comes first; the report names it and a rerun does not repeat it.
func TestV1ImportMemoryOffFollowsAnyMergedRoom(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		target string
		off    string // which of r1, r2 had memory off
	}{{"root, first off", V1TargetRoot, "r1"}, {"root, second off", V1TargetRoot, "r2"}, {"sub, first off", V1TargetSub, "r1"}, {"sub, second off", V1TargetSub, "r2"}} {
		source, base := v1Fixture(t)
		addV1Rooms(t, source, []string{"r1", "r2"}, map[int64]string{10: "r1", 11: "r2"})
		db := openV1Fixture(t, source)
		if _, err := db.Exec(`UPDATE rooms SET data=json_set(data,'$.memory_enabled',json('false')) WHERE id=?`, c.off); err != nil {
			t.Fatal(err)
		}
		s := newV1RoleStore(t)
		mapping := targetMapping(t, t.TempDir(), base)
		mapping.Rooms = map[string]V1ImportRoom{
			"r1": {Project: "proj", Target: c.target, Title: "general", Lifecycle: "open"}, "r2": {Project: "proj", Target: c.target, Title: "general", Lifecycle: "open"},
			"dm:alice:bot@repo": {Project: "repo", Skip: true}, "work": {Project: "repo", Skip: true},
		}
		report, err := s.ImportV1(context.Background(), source, mapping, false)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var memory sql.NullBool
		if err := s.db.QueryRow(`SELECT r.memory_enabled FROM message m JOIN room r ON r.id=m.room_id WHERE m.seq=10`).Scan(&memory); err != nil || !memory.Valid || memory.Bool {
			t.Errorf("%s: the target must have memory off: %v %v", c.name, memory, err)
		}
		if len(report.MemoryOffTargets) != 1 || !strings.Contains(report.MemoryOffTargets[0], "because v1 room "+c.off+" had it off") || report.Imported["room_memory_settings"] != 1 {
			t.Errorf("%s: the report names it: %v %v", c.name, report.MemoryOffTargets, report.Imported)
		}
		again, err := s.ImportV1(context.Background(), source, mapping, false)
		if err != nil || len(again.MemoryOffTargets) != 0 || again.Imported["room_memory_settings"] != 0 {
			t.Errorf("%s: a rerun repeats nothing: %v %v %v", c.name, err, again.MemoryOffTargets, again.Imported)
		}
	}
}

// A project alias and a sub-room title may both hold ':'; ("a", "b:c") and ("a:b", "c") are different targets, each in its own project.
func TestV1ImportSharedRoomKeysDoNotCollide(t *testing.T) {
	t.Parallel()
	source, base := v1Fixture(t)
	addV1Rooms(t, source, []string{"r1", "r2"}, map[int64]string{10: "r1", 11: "r2"})
	s := newV1RoleStore(t)
	root := t.TempDir()
	mapping := base
	mapping.Projects = map[string]V1ImportProject{"repo": base.Projects["repo"], "a": {Name: "A", Folder: root + "/a"}, "a:b": {Name: "AB", Folder: root + "/ab"}}
	mapping.Rooms = map[string]V1ImportRoom{
		"r1": {Project: "a", Target: V1TargetSub, Title: "b:c", Lifecycle: "open"}, "r2": {Project: "a:b", Target: V1TargetSub, Title: "c", Lifecycle: "open"},
		"dm:alice:bot@repo": {Project: "repo", Skip: true}, "work": {Project: "repo", Skip: true},
	}
	if _, err := s.ImportV1(context.Background(), source, mapping, false); err != nil {
		t.Fatal(err)
	}
	var p1, p2, r1, r2 string
	s.db.QueryRow(`SELECT r.project_id,r.id FROM message m JOIN room r ON r.id=m.room_id WHERE m.seq=10`).Scan(&p1, &r1)
	s.db.QueryRow(`SELECT r.project_id,r.id FROM message m JOIN room r ON r.id=m.room_id WHERE m.seq=11`).Scan(&p2, &r2)
	if r1 == r2 || p1 == p2 || p1 == "" || p2 == "" {
		t.Fatalf("each target is its own room in its own project: %s/%s %s/%s", p1, r1, p2, r2)
	}
}

// Every project starts with a "general" task room under its home room, the imported ones too. Importing into a v2 project that already
// has its own "general" fills that room instead of adding a second one, and a project the import creates gets one even when no v1 room
// is placed in a sub-room.
func TestV1ImportReusesTheProjectsGeneralRoom(t *testing.T) {
	t.Parallel()
	source, base := v1Fixture(t)
	addV1Rooms(t, source, []string{"proj", "proj-leader", "extra"}, map[int64]string{10: "proj-leader", 11: "proj", 12: "extra", 13: "proj-leader"})
	s := newV1RoleStore(t)
	ctx := context.Background()
	// The project is made in v2 during the first run, after the history numbers were reserved.
	if _, err := s.ReserveV1MessageIDs(ctx, source); err != nil {
		t.Fatal(err)
	}
	project, err := s.CreateProject(ctx, Project{Name: "proj", Folder: "/tmp/proj"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: "proj", CreatedBy: "usr_owner"})
	if err != nil {
		t.Fatal(err)
	}
	general, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: home.ID, Kind: "task", Title: GeneralRoomTitle, CreatedBy: "usr_owner"})
	if err != nil {
		t.Fatal(err)
	}
	mapping := targetMapping(t, t.TempDir(), base)
	mapping.Projects["proj"] = V1ImportProject{ExistingID: project.ID}
	if _, err := s.ImportV1(ctx, source, mapping, false); err != nil {
		t.Fatalf("import: %v", err)
	}
	var generals int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room WHERE project_id=? AND title='general'`, project.ID).Scan(&generals); err != nil || generals != 1 {
		t.Fatalf("general rooms in the existing project: %d %v", generals, err)
	}
	for _, number := range []int64{11, 12} {
		var roomID string
		if err := s.db.QueryRow(`SELECT room_id FROM message WHERE seq=?`, number).Scan(&roomID); err != nil || roomID != general.ID {
			t.Errorf("message %d should be in the project's own general room %s, got %s (%v)", number, general.ID, roomID, err)
		}
	}
	var repoGeneral int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room r JOIN project p ON p.id=r.project_id JOIN room h ON h.id=r.parent_id AND h.kind='home'
		WHERE p.name='Imported' AND r.kind='task' AND r.title='general' AND r.lifecycle='open'`).Scan(&repoGeneral); err != nil || repoGeneral != 1 {
		t.Fatalf("an imported project starts with one open general task room: %d %v", repoGeneral, err)
	}
	// A rerun adds nothing twice.
	if _, err := s.ImportV1(ctx, source, mapping, false); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room WHERE title='general'`).Scan(&generals); err != nil || generals != 2 {
		t.Fatalf("general rooms after a rerun: %d %v", generals, err)
	}
}
