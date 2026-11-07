package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGlobalKnowledgeAccessCapsAndDelivery(t *testing.T) {
	t.Parallel()
	s, project, task, agent := knowledgeFixture(t)
	ctx := context.Background()
	profile, err := s.ManageGlobalKnowledge(ctx, "user_profile", "usr_owner", project.ID, "user", "usr_owner", "", 0, "review preference", false)
	if err != nil {
		t.Fatal(err)
	}
	var eventActor string
	if err := s.db.QueryRow("SELECT json_extract(payload_json,'$.actor_id') FROM event WHERE kind='global_knowledge_created' LIMIT 1").Scan(&eventActor); err != nil || eventActor != "usr_owner" {
		t.Fatalf("global knowledge event actor: %q %v", eventActor, err)
	}
	if _, err := s.ManageGlobalKnowledge(ctx, "user_profile", "usr_owner", project.ID, "user", "usr_viewer", "", 0, "no", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer write: %v", err)
	}
	if _, err := s.db.Exec("UPDATE membership SET role='operator' WHERE project_id=? AND user_id='usr_viewer'", project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageGlobalKnowledge(ctx, "user_profile", "usr_owner", project.ID, "user", "usr_viewer", "", 0, "no", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("project operator changed global profile: %v", err)
	}
	if _, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, "", 0, "no", false); !errors.Is(err, ErrMemoryDisabled) {
		t.Fatalf("agent write with memory off: %v", err)
	}
	if _, err := s.ManageGlobalKnowledge(ctx, "user_profile", "usr_owner", project.ID, "user", "usr_owner", "", 0, strings.Repeat("x", 2049), false); !errors.Is(err, ErrKnowledgeLimit) {
		t.Fatalf("profile cap: %v", err)
	}
	if _, err := s.ManageGlobalKnowledge(ctx, "user_profile", "usr_owner", project.ID, "user", "usr_owner", profile.ID, 9, "changed", false); !errors.Is(err, ErrStaleRev) {
		t.Fatalf("stale edit: %v", err)
	}
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	note, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, "", 0, "shared preference", false)
	if err != nil {
		t.Fatal(err)
	}
	if note.Active {
		t.Fatal("a note an agent adds to the shared notes must wait for approval")
	}
	if _, err := s.SetGlobalKnowledgeActive(ctx, "agent_shared_notes", "global", "usr_owner", note.ID, note.Rev, true); err != nil {
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
	if len(snapshot["user_profiles"].([]KnowledgeRecord)) != 1 || len(snapshot["agent_shared_notes"].([]KnowledgeRecord)) != 1 {
		t.Fatalf("global knowledge missing from turn context: %+v", snapshot)
	}
	// Only active global records are delivered, so each must say so: an agent that reads "active": false
	// treats the record as suspect or stale.
	for _, key := range []string{"user_profiles", "agent_shared_notes"} {
		for _, record := range snapshot[key].([]KnowledgeRecord) {
			if !record.Active {
				t.Fatalf("%s record %s reaches the turn context marked inactive", key, record.ID)
			}
		}
	}
	hits, err := s.RecallKnowledge(ctx, agent.ID, "preference", 10)
	if err != nil {
		t.Fatal(err)
	}
	scopes := map[string]bool{}
	for _, hit := range hits {
		scopes[hit.Source] = true
	}
	if !scopes["user_profile"] || !scopes["agent_shared_notes"] {
		t.Fatalf("global recall missing scopes: %+v", hits)
	}
}

// What an agent writes to the notes every project's agents read waits for the owner, a change to an approved note waits again, and an agent cannot edit a
// note that is waiting.
func TestAgentSharedNotesWaitForApprovalAndStaySilentUntilThen(t *testing.T) {
	t.Parallel()
	s, project, task, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	note, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, "", 0, "use tabs", false)
	if err != nil || note.Active {
		t.Fatalf("a new agent note: %+v err=%v", note, err)
	}
	shared := func() int {
		t.Helper()
		var snapshot map[string]any
		if err := s.Tx(ctx, func(tx *Tx) (err error) { snapshot, err = tx.knowledgeSnapshot(project.ID, task.ID); return err }); err != nil {
			t.Fatal(err)
		}
		return len(snapshot["agent_shared_notes"].([]KnowledgeRecord))
	}
	if n := shared(); n != 0 {
		t.Fatalf("a note that waits reached an agent's context: %d", n)
	}
	// The agent that wrote a waiting note may still fix it (and take it back), so nothing it wrote is stuck; what it edits is still waiting.
	fixed, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, note.ID, note.Rev, "use spaces", false)
	if err != nil || fixed.Active {
		t.Fatalf("an agent fixing its own waiting note: %+v err=%v", fixed, err)
	}
	note = fixed
	approved, err := s.SetGlobalKnowledgeActive(ctx, "agent_shared_notes", "global", "usr_owner", note.ID, note.Rev, true)
	if err != nil || !approved.Active {
		t.Fatalf("approval: %+v err=%v", approved, err)
	}
	if n := shared(); n != 1 {
		t.Fatalf("an approved note must reach an agent's context: %d", n)
	}
	changed, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, note.ID, approved.Rev, "use spaces", false)
	if err != nil || changed.Active {
		t.Fatalf("an agent changing an approved note: %+v err=%v", changed, err)
	}
	if n := shared(); n != 0 {
		t.Fatalf("the changed words reached an agent's context before anyone approved them: %d", n)
	}
	// The owner's own notes are live at once.
	if own, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "user", "usr_owner", "", 0, "owner's note", false); err != nil || !own.Active {
		t.Fatalf("an owner's note: %+v err=%v", own, err)
	}
	if n := shared(); n != 1 {
		t.Fatalf("the owner's note must be live at once: %d", n)
	}
}

// A person's profile is the business of the projects that person belongs to: an agent reads and edits it only for a member of its own project, and the
// context it starts with carries the profiles of that project's members and no others.
func TestAgentsSeeAndEditOnlyTheProfilesOfTheirProjectsMembers(t *testing.T) {
	t.Parallel()
	s, project, task, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_stranger','stranger','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"usr_owner", "usr_stranger"} {
		if _, err := s.ManageGlobalKnowledge(ctx, "user_profile", id, project.ID, "user", "usr_owner", "", 0, "profile of "+id, false); err != nil {
			t.Fatal(err)
		}
	}
	var snapshot map[string]any
	if err := s.Tx(ctx, func(tx *Tx) (err error) { snapshot, err = tx.knowledgeSnapshot(project.ID, task.ID); return err }); err != nil {
		t.Fatal(err)
	}
	profiles := snapshot["user_profiles"].([]KnowledgeRecord)
	if len(profiles) != 1 || profiles[0].ScopeID != "usr_owner" {
		t.Fatalf("the context should carry the member's profile and not the stranger's: %+v", profiles)
	}
	// Editing: a member's yes, a stranger's no, and so is a member who has been disabled.
	if _, err := s.ManageGlobalKnowledge(ctx, "user_profile", "usr_owner", project.ID, "agent", agent.ID, "", 0, "agent note about a member", false); err != nil {
		t.Fatalf("a member's profile: %v", err)
	}
	if _, err := s.ManageGlobalKnowledge(ctx, "user_profile", "usr_stranger", project.ID, "agent", agent.ID, "", 0, "agent note about a stranger", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger's profile: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE user SET disabled_at='now' WHERE id='usr_viewer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageGlobalKnowledge(ctx, "user_profile", "usr_viewer", project.ID, "agent", agent.ID, "", 0, "note about a disabled member", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a disabled member's profile: %v", err)
	}
	for id, want := range map[string]bool{"usr_owner": true, "usr_stranger": false, "usr_viewer": false} {
		if got, err := s.IsProjectMember(ctx, project.ID, id); err != nil || got != want {
			t.Fatalf("IsProjectMember(%s) = %v, %v; want %v", id, got, err, want)
		}
	}
}

// An agent edits and deletes only the shared notes it wrote: not the owner's, not another agent's. (Editing an approved one switched it off for every
// project until the owner approved it again.)
func TestAgentsTouchOnlyTheSharedNotesTheyWrote(t *testing.T) {
	t.Parallel()
	s, project, _, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	owners, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "user", "usr_owner", "", 0, "the owner's rule", false)
	if err != nil || !owners.Active {
		t.Fatalf("owner note: %+v %v", owners, err)
	}
	if _, err := s.db.Exec(`INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,created_at,updated_at,active) VALUES ('knw_other','agent_shared_notes','global','another agent''s note',1,'agent','agt_someone_else',?,?,1)`, timestamp(), timestamp()); err != nil {
		t.Fatal(err)
	}
	for name, note := range map[string]KnowledgeRecord{"the owner's note": owners, "another agent's note": {ID: "knw_other", Rev: 1}} {
		if _, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, note.ID, note.Rev, "vandalised", false); !errors.Is(err, ErrForbidden) {
			t.Errorf("an agent edited %s: %v", name, err)
		}
		if _, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, note.ID, note.Rev, "", true); !errors.Is(err, ErrForbidden) {
			t.Errorf("an agent deleted %s: %v", name, err)
		}
	}
	var live int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM global_memory WHERE scope='agent_shared_notes' AND active=1`).Scan(&live); err != nil || live != 2 {
		t.Fatalf("both foreign notes must still be live: %d %v", live, err)
	}
	// Its own note it can edit and delete, waiting or approved.
	mine, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, "", 0, "my note", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, mine.ID, mine.Rev, "", true); err != nil {
		t.Fatalf("an agent deleting its own waiting note: %v", err)
	}
}

// Notes waiting for approval do not use the shared notes' cap (an agent could otherwise fill it with notes nobody has read, and block the owner's), and
// each agent's waiting notes have a cap of their own. Approving a note counts it.
func TestWaitingSharedNotesDoNotFillTheCap(t *testing.T) {
	t.Parallel()
	s, project, _, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	cap, _ := globalKnowledgeCap("agent_shared_notes")
	chunk := strings.Repeat("x", cap/4)
	var waiting []KnowledgeRecord
	for used := 0; used+len(chunk) <= cap; used += len(chunk) {
		note, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, "", 0, chunk, false)
		if err != nil {
			t.Fatalf("a waiting note within the agent's own cap: %v", err)
		}
		waiting = append(waiting, note)
	}
	if _, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, "", 0, "one too many", false); !errors.Is(err, ErrKnowledgeLimit) {
		t.Fatalf("an agent's waiting notes are capped: %v", err)
	}
	// The owner is not blocked by them.
	if _, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "user", "usr_owner", "", 0, "the owner's note", false); err != nil {
		t.Fatalf("waiting notes blocked the owner: %v", err)
	}
	// The agent can take its own back, which frees its own cap.
	if _, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, waiting[0].ID, waiting[0].Rev, "", true); err != nil {
		t.Fatal(err)
	}
	again, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, "", 0, strings.Repeat("y", cap/4), false)
	if err != nil {
		t.Fatalf("after taking one back: %v", err)
	}
	waiting = append(waiting, again)
	// Approving counts the note against the shared cap: approving more than fits is refused.
	var refused error
	for _, note := range waiting[1:] {
		if _, err := s.SetGlobalKnowledgeActive(ctx, "agent_shared_notes", "global", "usr_owner", note.ID, note.Rev, true); err != nil {
			refused = err
			break
		}
	}
	if !errors.Is(refused, ErrKnowledgeLimit) && len(waiting) > 1 {
		t.Fatalf("approving notes beyond the shared cap must be refused: %v", refused)
	}
}

// An agent's recall finds the profiles of its own project's people only: the same rule as the context it starts with, and as editing.
func TestRecallFindsOnlyTheProfilesOfTheProjectsMembers(t *testing.T) {
	t.Parallel()
	s, project, _, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_stranger','stranger','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	for id, body := range map[string]string{"usr_owner": "the member prefers zebra stripes", "usr_stranger": "the stranger prefers zebra spots"} {
		if _, err := s.ManageGlobalKnowledge(ctx, "user_profile", id, project.ID, "user", "usr_owner", "", 0, body, false); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.RecallKnowledge(ctx, agent.ID, "zebra", 10)
	if err != nil {
		t.Fatal(err)
	}
	var profiles []string
	for _, hit := range hits {
		if hit.Source == "user_profile" {
			profiles = append(profiles, hit.Body)
		}
	}
	if len(profiles) != 1 || !strings.Contains(profiles[0], "member prefers") {
		t.Fatalf("recall must return the member's profile and not the stranger's: %v", profiles)
	}
}
