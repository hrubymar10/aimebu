package store

import (
	"context"
	"errors"
	"testing"
)

// Only people who can approve the note (the owner, the admins) see its inbox card; a project owner who is neither does not, and cannot answer it
// either. The card is withdrawn when the note's writer takes it back and resolved by an approval.
func TestNoteApprovalCardIsForThoseWhoCanApprove(t *testing.T) {
	t.Parallel()
	s, project, _, agent := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.SetProjectMemoryEnabled(ctx, project.ID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	// A project owner who is not an admin, and an admin who owns the project too.
	for _, user := range []struct{ id, admin string }{{"usr_pm", "0"}, {"usr_admin", "1"}} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at,admin) VALUES (?,?,'hash','{}',?,?)`, user.id, user.id, timestamp(), user.admin); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, project.ID, user.id); err != nil {
			t.Fatal(err)
		}
	}
	note, err := s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", project.ID, "agent", agent.ID, "", 0, "use tabs", false)
	if err != nil {
		t.Fatal(err)
	}
	cards := func(user string) []InboxEntry {
		t.Helper()
		entries, err := s.InboxForUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		var found []InboxEntry
		for _, entry := range entries {
			if entry.NoteID != "" {
				found = append(found, entry)
			}
		}
		return found
	}
	if len(cards("usr_owner")) != 1 || len(cards("usr_admin")) != 1 || len(cards("usr_pm")) != 0 || len(cards("usr_viewer")) != 0 {
		t.Fatalf("the card is for the owner and admins only: owner=%d admin=%d pm=%d viewer=%d", len(cards("usr_owner")), len(cards("usr_admin")), len(cards("usr_pm")), len(cards("usr_viewer")))
	}
	card := cards("usr_owner")[0]
	// Someone who cannot approve cannot answer it, and nothing changes.
	if err := s.DecideNoteApproval(ctx, card.ID, "usr_pm", "approve", card.Rev); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin answering: %v", err)
	}
	if err := s.DecideNoteApproval(ctx, card.ID, "usr_owner", "approve", card.Rev+5); !errors.Is(err, ErrStaleInboxRev) {
		t.Fatalf("a stale card: %v", err)
	}
	if err := s.DecideNoteApproval(ctx, card.ID, "usr_owner", "maybe", card.Rev); !errors.Is(err, ErrChoiceNotOffered) {
		t.Fatalf("a choice the card does not offer: %v", err)
	}
	if len(cards("usr_owner")) != 1 {
		t.Fatal("refused answers must leave the card open")
	}
	// An approval through the memory viewer's own toggle closes the card as well.
	if _, err := s.SetGlobalKnowledgeActive(ctx, "agent_shared_notes", "global", "usr_admin", note.ID, note.Rev, true); err != nil {
		t.Fatal(err)
	}
	if len(cards("usr_owner")) != 0 {
		t.Fatal("approving the note must close its card")
	}
	if err := s.DecideNoteApproval(ctx, card.ID, "usr_owner", "approve", card.Rev); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("answering a closed card: %v", err)
	}
}
