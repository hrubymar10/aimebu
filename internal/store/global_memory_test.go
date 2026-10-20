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
	if _, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, "", 0, "shared preference", false); err != nil {
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
