package store

import (
	"context"
	"strings"
	"testing"
)

func TestCredentialAlertIsAnOwnerOnlySystemEvent(t *testing.T) {
	t.Parallel()
	s, _, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordCredentialAlert(ctx, "login_imported", "claude: login imported into profile \"work\""); err != nil {
		t.Fatal(err)
	}
	events, err := s.RecentSystemEvents(ctx, owner, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "credential_alert" {
			found = strings.Contains(event.PayloadJSON, "login_imported") && strings.Contains(event.PayloadJSON, "work")
		}
	}
	if !found {
		t.Fatalf("the owner does not see the alert: %+v", events)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_member','member','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if other, err := s.RecentSystemEvents(ctx, "usr_member", 20); err != nil || len(other) != 0 {
		t.Fatalf("a non-owner sees a project-less credential event: %+v %v", other, err)
	}
}
