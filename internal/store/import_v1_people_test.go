package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The person who has the owner's own name is suggested into the owner's account; with several people writing, everyone else is a
// historical label until the owner decides (ResolvePeople then gives each an account to start from).
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
	// The suggestion the wizard shows is the same one the API gives: a new viewer account for each person who is not the owner.
	preview.DestinationUsers = []string{"owner"}
	preview.ResolvePeople()
	if got := preview.Suggested.NewUsers["alice"]; got != (V1NewUser{Login: "alice", Role: "viewer"}) || preview.Suggested.Humans["alice"] != "alice" {
		t.Errorf("alice's suggested account: %+v, mapped to %q", got, preview.Suggested.Humans["alice"])
	}
	if _, made := preview.Suggested.NewUsers["Owner"]; made || preview.Suggested.Humans["Owner"] != "owner" {
		t.Errorf("the owner's counterpart needs no new account: %+v, mapped to %q", preview.Suggested.NewUsers, preview.Suggested.Humans["Owner"])
	}
}

// On an install with only one person, that person is the importing owner whatever the v1 name was: mapping them anywhere
// else would leave their read positions, memory profile, pins and reactions behind.
func TestV1PreviewSuggestsTheOnlyPersonAsTheOwner(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if got := preview.Suggested.Humans["alice"]; got != "owner" {
		t.Fatalf("the only person in the backup is the owner: %q", got)
	}
	preview.ResolvePeople()
	if len(preview.Suggested.NewUsers) != 0 {
		t.Fatalf("no new account is suggested for the owner's counterpart: %+v", preview.Suggested.NewUsers)
	}
}

// With two people writing there is no way to tell which is the owner, so neither is merged into the owner's account unasked.
func TestV1PreviewDoesNotGuessWhenSeveralPeopleWrote(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	db := openV1Fixture(t, source)
	if _, err := db.Exec(`INSERT INTO agents(id,data) VALUES ('bob','{"id":"bob","kind":"human","name":"bob"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(id,room_id,data) VALUES (98,'work','{"id":98,"room_id":"work","from":"bob","from_kind":"human","body":"hi","created_at":"2026-10-01T12:00:00Z"}')`); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Suggested.Humans["alice"] != "-" || preview.Suggested.Humans["bob"] != "-" {
		t.Fatalf("two writers, no guess: %+v", preview.Suggested.Humans)
	}
}

// What a skipped person would lose is counted per person, so the wizard can say it: memory profiles, reactions, read positions, and the
// rooms they hid or pinned. People who only have agent or room-preference rows are listed too.
func TestV1PreviewCountsWhatASkippedPersonWouldLose(t *testing.T) {
	t.Parallel()
	source, _ := v1RoomPrefsFixture(t)
	db := openV1Fixture(t, source)
	if _, err := db.Exec(`UPDATE agents SET data='{"id":"alice","kind":"human","name":"alice","read_cursors":{"work":2,"elsewhere":0,"third":5}}' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	// Every listed person gets stakes, including bob and carol, who wrote nothing; an AI is never a person.
	want := V1HumanStakes{MemoryProfiles: 1, Reactions: 1, ReadPositions: 2, RoomView: 3}
	if got := preview.HumanStakes["alice"]; got != want {
		t.Errorf("alice would lose %+v, want %+v", got, want)
	}
	for human, roomView := range map[string]int{"bob": 1, "carol": 1} {
		if got := preview.HumanStakes[human]; got.RoomView != roomView {
			t.Errorf("%s would lose %+v, want room view %d", human, got, roomView)
		}
	}
	if len(preview.HumanStakes) != len(preview.Humans) {
		t.Errorf("stakes for %d people, %d listed: %+v", len(preview.HumanStakes), len(preview.Humans), preview.HumanStakes)
	}
	if _, bot := preview.HumanStakes["bot@repo"]; bot {
		t.Errorf("an agent is not a person: %+v", preview.HumanStakes)
	}
}

// A legacy human with no messages or membership still prevents the writer
// from being guessed as the owner. Agent rows carry read cursors, while older
// backups can also leave a room preference without an agent row.
func TestV1PreviewCountsPeopleOutsideConversationRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		setup    []string
		readWant int
		viewWant int
	}{
		{"agent only", []string{`INSERT INTO agents(id,data) VALUES ('bob','{"id":"bob","kind":"human","name":"bob"}')`}, 0, 0},
		{"read cursor only", []string{`INSERT INTO agents(id,data) VALUES ('bob','{"id":"bob","kind":"human","name":"bob","read_cursors":{"work":2}}')`}, 1, 0},
		{"room preference only", []string{
			`CREATE TABLE room_prefs (agent_id TEXT NOT NULL, room_id TEXT NOT NULL, hidden INTEGER NOT NULL DEFAULT 0, pinned INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (agent_id, room_id))`,
			`INSERT INTO room_prefs VALUES ('bob','work',1,0)`,
		}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source, _ := v1Fixture(t)
			db := openV1Fixture(t, source)
			for _, statement := range tc.setup {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			preview, err := PreviewV1Import(context.Background(), source, "owner")
			if err != nil {
				t.Fatal(err)
			}
			if preview.Counts["humans"] != 2 || len(preview.Humans) != 2 || preview.Suggested.Humans["alice"] != "-" || preview.Suggested.Humans["bob"] != "-" {
				t.Fatalf("a quiet person must prevent the owner guess: %+v", preview)
			}
			if got := preview.HumanStakes["bob"]; got.ReadPositions != tc.readWant || got.RoomView != tc.viewWant {
				t.Fatalf("bob's read and view stakes: %+v, want %d and %d", got, tc.readWant, tc.viewWant)
			}
		})
	}
}

// A new account's login is derived from the v1 name the way the wizard does it, and never collides with an existing login or another person.
func TestV1SuggestedLoginMatchesTheWizard(t *testing.T) {
	t.Parallel()
	taken := map[string]bool{"owner": true, "alice": true}
	for name, want := range map[string]string{
		"Bob":       "bob",
		"alice":     "alice-2",
		"Zoë Smith": "zo-smith",
		"7up":       "user-up",
		"x":         "user-x",
		"12":        "user",
		"a.b.c":     "a-b-c",
		"Owner":     "owner-2",
	} {
		login := v1SuggestedLogin(name, taken)
		if login != want {
			t.Errorf("%q gets %q, want %q", name, login, want)
		}
		taken[login] = true
	}
	long := v1SuggestedLogin(strings.Repeat("a", 40), map[string]bool{})
	if len(long) != 28 {
		t.Errorf("a long name is cut to 28 characters: %q", long)
	}
}

// One person wrote, but others exist in the data (a member, a reaction, a memory profile): the writer may be a colleague who simply talks
// the most, so nobody is merged into the owner's account.
func TestV1PreviewDoesNotGuessWhenOtherPeopleNeverWrote(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	db := openV1Fixture(t, source)
	if _, err := db.Exec(`INSERT INTO agents(id,data) VALUES ('bob','{"id":"bob","kind":"human","name":"bob"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE rooms SET data=json_set(data,'$.members',json_insert(json_extract(data,'$.members'),'$[#]','bob')) WHERE id='work'`); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Humans) < 2 {
		t.Fatalf("fixture should have a second person: %+v", preview.Humans)
	}
	for human, login := range preview.Suggested.Humans {
		if login == "owner" {
			t.Fatalf("%s was merged into the owner without a way to tell: %+v", human, preview.Suggested.Humans)
		}
	}
}

// Two spellings of the owner's name: the exact one wins, and otherwise the choice does not depend on map order.
func TestV1PreviewOwnerNameMatchIsDeterministic(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{[]string{"OWNER", "owner", "Owner"}, "owner"},
		{[]string{"OWNER", "Owner"}, "OWNER"},
	} {
		source, _ := v1Fixture(t)
		db := openV1Fixture(t, source)
		for i, name := range tc.names {
			if _, err := db.Exec(`INSERT INTO agents(id,data) VALUES (?,?)`, name, `{"id":"`+name+`","kind":"human","name":"`+name+`"}`); err != nil {
				t.Fatal(err)
			}
			id := 200 + i
			if _, err := db.Exec(`INSERT INTO messages(id,room_id,data) VALUES (?,'work',?)`, id, fmt.Sprintf(`{"id":%d,"room_id":"work","from":%q,"from_kind":"human","body":"hi","created_at":"2026-10-01T12:00:00Z"}`, id, name)); err != nil {
				t.Fatal(err)
			}
		}
		for run := 0; run < 20; run++ {
			preview, err := PreviewV1Import(context.Background(), source, "owner")
			if err != nil {
				t.Fatal(err)
			}
			for human, login := range preview.Suggested.Humans {
				if (login == "owner") != (human == tc.want) {
					t.Fatalf("names %v: %s -> %q, want only %s mapped to the owner", tc.names, human, login, tc.want)
				}
			}
		}
	}
}
