package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

func knowledgeFixture(t *testing.T) (*Store, Project, Room, Agent) {
	t.Helper()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	for _, id := range []string{"usr_owner", "usr_viewer"} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?,'hash','{}',?)`, id, id, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_owner','owner'),(?,'usr_viewer','viewer')`, f.project.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "Task", CreatedBy: "usr_owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, task.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	return s, f.project, task, f.agent
}

func TestFirstNormalTurnCarriesCurrentKnowledge(t *testing.T) {
	s, project, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE settings_project SET memory_enabled=1 WHERE project_id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageKnowledge(ctx, "project", project.ID, "agent", agent.ID, "", 0, "launch checklist", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageKnowledge(ctx, "task", room.ID, "agent", agent.ID, "", 0, "task checkpoint", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "start now"}); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
	if err != nil || !ready {
		t.Fatalf("first dispatch ready=%t err=%v", ready, err)
	}
	var contextData struct {
		Knowledge struct {
			Enabled bool              `json:"project_memory_enabled"`
			Project []KnowledgeRecord `json:"project_memory"`
			Task    []KnowledgeRecord `json:"task_notes"`
		} `json:"knowledge"`
	}
	if err := json.Unmarshal(delivery.Context, &contextData); err != nil {
		t.Fatal(err)
	}
	if !contextData.Knowledge.Enabled || len(contextData.Knowledge.Project) != 1 || len(contextData.Knowledge.Task) != 1 {
		t.Fatalf("first turn knowledge=%+v", contextData.Knowledge)
	}
}

func TestKnowledgeAccessRevisionsAndSnapshot(t *testing.T) {
	t.Parallel()
	s, project, task, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.ManageKnowledge(ctx, "project", project.ID, "user", "usr_owner", "", 0, "release checklist", false); !errors.Is(err, ErrMemoryDisabled) {
		t.Fatalf("write while off: %v", err)
	}
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_viewer", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer setting: %v", err)
	}
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	fact, err := s.ManageKnowledge(ctx, "project", project.ID, "user", "usr_owner", "", 0, "release checklist", false)
	if err != nil || fact.Rev != 1 || fact.AuthorID != "usr_owner" {
		t.Fatalf("fact=%+v err=%v", fact, err)
	}
	if _, err := s.ManageKnowledge(ctx, "project", project.ID, "user", "usr_viewer", "", 0, "secret", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer write: %v", err)
	}
	updated, err := s.ManageKnowledge(ctx, "project", project.ID, "agent", agent.ID, fact.ID, fact.Rev, "updated release checklist", false)
	if err != nil || updated.Rev != 2 || updated.AuthorID != agent.ID {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if _, err := s.ManageKnowledge(ctx, "project", project.ID, "user", "usr_owner", fact.ID, 1, "stale", false); !errors.Is(err, ErrStaleRev) {
		t.Fatalf("stale edit: %v", err)
	}
	if _, err := s.ManageKnowledge(ctx, "task", task.ID, "user", "usr_viewer", "", 0, "viewer edit", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer task note: %v", err)
	}
	note, err := s.ManageKnowledge(ctx, "task", task.ID, "agent", agent.ID, "", 0, "task next step", false)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		snapshot, err = tx.knowledgeSnapshot(project.ID, task.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(snapshot)
	if !strings.Contains(string(encoded), "updated release checklist") || !strings.Contains(string(encoded), "task next step") {
		t.Fatalf("snapshot=%s", encoded)
	}
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		snapshot, err = tx.knowledgeSnapshot(project.ID, task.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(snapshot)
	if strings.Contains(string(encoded), "release checklist") || !strings.Contains(string(encoded), "task next step") {
		t.Fatalf("snapshot while off=%s", encoded)
	}
	if _, err := s.ManageKnowledge(ctx, "task", task.ID, "user", "usr_owner", note.ID, note.Rev, "", true); err != nil {
		t.Fatal(err)
	}
	if records, err := s.ListKnowledge(ctx, "task", task.ID); err != nil || len(records) != 0 {
		t.Fatalf("after delete=%+v err=%v", records, err)
	}
}

func TestKnowledgeListNamesTheAuthor(t *testing.T) {
	t.Parallel()
	s, project, task, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	byPerson, err := s.ManageKnowledge(ctx, "project", project.ID, "user", "usr_owner", "", 0, "written by a person", false)
	if err != nil {
		t.Fatal(err)
	}
	byAgent, err := s.ManageKnowledge(ctx, "task", task.ID, "agent", agent.ID, "", 0, "written by an agent", false)
	if err != nil {
		t.Fatal(err)
	}
	people, err := s.ListKnowledge(ctx, "project", project.ID)
	if err != nil || len(people) != 1 || people[0].ID != byPerson.ID || people[0].AuthorName != "usr_owner" {
		t.Fatalf("a person's record names the login: %+v err=%v", people, err)
	}
	notes, err := s.ListKnowledge(ctx, "task", task.ID)
	if err != nil || len(notes) != 1 || notes[0].ID != byAgent.ID || notes[0].AuthorName == "" || notes[0].AuthorName == agent.ID {
		t.Fatalf("an agent's record names its slug, not its id: %+v err=%v", notes, err)
	}
}

func TestKnowledgeRecallRespectsRoomMembership(t *testing.T) {
	t.Parallel()
	s, project, task, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageKnowledge(ctx, "project", project.ID, "user", "usr_owner", "", 0, "lunar project fact", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageKnowledge(ctx, "task", task.ID, "agent", agent.ID, "", 0, "lunar task note", false); err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "Other", CreatedBy: "usr_owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: task.ID, AuthorKind: "user", AuthorID: "usr_owner", Body: "lunar visible message"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: other.ID, AuthorKind: "user", AuthorID: "usr_owner", Body: "lunar hidden message"}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.RecallKnowledge(ctx, agent.ID, "lunar", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits=%+v", hits)
	}
	for _, hit := range hits {
		if strings.Contains(hit.Body, "hidden") {
			t.Fatalf("unjoined room leaked: %+v", hit)
		}
	}
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecallKnowledge(ctx, agent.ID, "lunar", 10); !errors.Is(err, ErrMemoryDisabled) {
		t.Fatalf("memory off recall: %v", err)
	}
}

func TestRoomMemoryOverrideBlocksRecallAndSourcedWrites(t *testing.T) {
	s, project, task, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	message, err := s.InsertMessage(ctx, Message{RoomID: task.ID, AuthorKind: "user", AuthorID: "usr_owner", Body: "lunar source"})
	if err != nil {
		t.Fatal(err)
	}
	fact, err := s.ManageKnowledgeWithSource(ctx, "project", project.ID, "agent", agent.ID, "", 0, "lunar fact", false, message.ID)
	if err != nil || fact.SourceMessageID != message.ID {
		t.Fatalf("fact=%+v err=%v", fact, err)
	}
	fact, err = s.ManageKnowledge(ctx, "project", project.ID, "agent", agent.ID, fact.ID, fact.Rev, "lunar fact revised", false)
	if err != nil || fact.SourceMessageID != message.ID {
		t.Fatalf("revised provenance=%+v err=%v", fact, err)
	}
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "Other", CreatedBy: "usr_owner"})
	if err != nil {
		t.Fatal(err)
	}
	otherMessage, err := s.InsertMessage(ctx, Message{RoomID: other.ID, AuthorKind: "user", AuthorID: "usr_owner", Body: "private source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageKnowledgeWithSource(ctx, "project", project.ID, "agent", agent.ID, "", 0, "blocked", false, otherMessage.ID); !errors.Is(err, ErrMemorySource) {
		t.Fatalf("unjoined source: %v", err)
	}
	if _, err := s.SetRoomMemoryEnabled(ctx, task.ID, "usr_viewer", new(bool)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer changed room: %v", err)
	}
	off := false
	if _, err := s.SetRoomMemoryEnabled(ctx, task.ID, "usr_owner", &off); err != nil {
		t.Fatal(err)
	}
	room, err := s.RoomByID(ctx, task.ID)
	if err != nil || room.MemoryEnabled == nil || *room.MemoryEnabled {
		t.Fatalf("room=%+v err=%v", room, err)
	}
	if _, err := s.ManageKnowledgeWithSource(ctx, "project", project.ID, "agent", agent.ID, "", 0, "blocked", false, message.ID); !errors.Is(err, ErrMemorySource) {
		t.Fatalf("source from disabled room: %v", err)
	}
	if _, err := s.ManageKnowledge(ctx, "project", project.ID, "agent", agent.ID, "", 0, "unsourced", false); err != nil {
		t.Fatal(err)
	}
	hits, err := s.RecallKnowledge(ctx, agent.ID, "lunar", 10)
	if err != nil || len(hits) != 1 || hits[0].Source != "project_memory" {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if _, err := s.SetRoomMemoryEnabled(ctx, task.ID, "usr_owner", nil); err != nil {
		t.Fatal(err)
	}
	hits, err = s.RecallKnowledge(ctx, agent.ID, "lunar", 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("inherit hits=%+v err=%v", hits, err)
	}
}

func TestKnowledgeSnippetCentresOnMatch(t *testing.T) {
	t.Parallel()
	short := "short body mentions Needle"
	if got := knowledgeSnippet(short, []string{"needle"}, "needle"); got != short {
		t.Fatalf("short body altered: %q", got)
	}
	body := strings.Repeat("界", 400) + " the NEEDLE is here " + strings.Repeat("x", 400)
	got := knowledgeSnippet(body, knowledgeTerms("needle"), "needle")
	if !strings.Contains(got, "NEEDLE") || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("snippet not centred on match: %q", got)
	}
	if n := len([]rune(got)); n != 220+2 {
		t.Fatalf("window = %d runes", n)
	}
	// A match near the end slides the window back so it stays full width.
	tail := strings.Repeat("a", 500) + " needle"
	got = knowledgeSnippet(tail, []string{"needle"}, "needle")
	if !strings.HasSuffix(got, "needle") || len([]rune(got)) != 220+1 {
		t.Fatalf("tail match: %d %q", len([]rune(got)), got)
	}
	// Falls back to a term when the whole query phrase is absent, else the head.
	body = strings.Repeat("b", 300) + " beta " + strings.Repeat("c", 300)
	if got := knowledgeSnippet(body, []string{"alpha", "beta"}, "alpha beta"); !strings.Contains(got, "beta") {
		t.Fatalf("term fallback: %q", got)
	}
	if got := knowledgeSnippet(body, []string{"zzz"}, "zzz"); !strings.HasPrefix(got, "bbb") || strings.HasPrefix(got, "…") {
		t.Fatalf("head fallback: %q", got)
	}
}

func TestTrimKnowledgeSnapshotForSlowHarness(t *testing.T) {
	t.Parallel()
	if !shouldTrimKnowledgeForAgent("pi", "ollama-cloud/Gemma4:31b") || shouldTrimKnowledgeForAgent("claude", "gemma") || shouldTrimKnowledgeForAgent("pi", "kimi") {
		t.Fatal("trim predicate must be pi + gemma only")
	}
	big := strings.Repeat("界", 1000)
	snapshot := map[string]any{
		"project_memory_enabled": true,
		"project_memory":         []KnowledgeRecord{{ID: "a", Body: "small"}, {ID: "b", Body: big}},
		"user_profiles":          []KnowledgeRecord{{ID: "c", Body: "dropped"}},
		"agent_shared_notes":     []KnowledgeRecord{},
	}
	got := trimKnowledgeSnapshotForSlowHarness(snapshot)
	records := got["project_memory"].([]KnowledgeRecord)
	if len(records) != 2 || records[0].Body != "small" || !strings.HasSuffix(records[1].Body, slowHarnessTruncationSuffix) || !utf8.ValidString(records[1].Body) {
		t.Fatalf("project memory: %+v", records)
	}
	total := 0
	for _, r := range records {
		total += len(r.Body)
	}
	if total > slowHarnessKnowledgeBodyCap {
		t.Fatalf("body bytes %d exceed cap", total)
	}
	if len(got["user_profiles"].([]KnowledgeRecord)) != 0 || got["project_memory_enabled"] != true {
		t.Fatalf("later records not dropped or flag lost: %+v", got)
	}
	if len(snapshot["project_memory"].([]KnowledgeRecord)[1].Body) != len(big) {
		t.Fatal("input snapshot was mutated")
	}
}
