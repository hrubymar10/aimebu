package store

import (
	"context"
	"testing"
)

// Only the person who has the owner's own name is suggested into the owner's account; everyone else is a historical label until the
// owner decides (the wizard then offers to make each an account).
func TestV1PreviewSuggestsOnlyTheOwnerByName(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	db := openV1Fixture(t, source)
	if _, err := db.Exec(`INSERT INTO messages(id,room_id,data) VALUES (99,'work','{"id":99,"room_id":"work","from":"Owner","from_kind":"human","body":"hi","created_at":"2026-10-01T12:00:00Z"}')`); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if got := preview.Suggested.Humans["Owner"]; got != "owner" {
		t.Errorf("the person named like the owner (any case) is the owner: %q", got)
	}
	if got := preview.Suggested.Humans["alice"]; got != "-" {
		t.Errorf("another person is not merged into the owner by default: %q", got)
	}
}
