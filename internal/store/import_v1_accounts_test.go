package store

import (
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func accountFixture(t *testing.T) (string, V1ImportMap) {
	t.Helper()
	source, base := v1Fixture(t)
	addV1Rooms(t, source, []string{"proj", "proj-leader", "extra"}, map[int64]string{10: "proj-leader", 11: "proj"})
	db := openV1Fixture(t, source)
	raw, _ := json.Marshal(map[string]any{"id": "bob", "kind": "human", "name": "bob"})
	if _, err := db.Exec(`INSERT INTO agents(id,data) VALUES ('bob',?)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	room, _ := json.Marshal(map[string]any{"id": "proj-leader", "members": []string{"alice", "bob"}, "created_at": "2026-10-01T12:00:00Z", "created_by": "alice"})
	if _, err := db.Exec(`UPDATE rooms SET data=? WHERE id='proj-leader'`, string(room)); err != nil {
		t.Fatal(err)
	}
	msg, _ := json.Marshal(map[string]any{"id": 12, "room_id": "proj-leader", "from": "bob", "from_kind": "human", "body": "from bob", "created_at": "2026-10-01T12:00:00Z"})
	if _, err := db.Exec(`INSERT INTO messages(id,room_id,data) VALUES (12,'proj-leader',?)`, string(msg)); err != nil {
		t.Fatal(err)
	}
	mapping := targetMapping(t, t.TempDir(), base)
	mapping.Humans["bob"] = "bobby"
	mapping.NewUsers = map[string]V1NewUser{"bob": {Login: "bobby", Role: "operator"}}
	return source, mapping
}

func TestV1ImportCreatesAccountsForV1People(t *testing.T) {
	t.Parallel()
	source, mapping := accountFixture(t)
	s := newV1RoleStore(t)
	ctx := context.Background()
	var users int
	s.db.QueryRow(`SELECT COUNT(*) FROM user`).Scan(&users)
	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var afterDry int
	s.db.QueryRow(`SELECT COUNT(*) FROM user`).Scan(&afterDry)
	if afterDry != users || len(dry.NewAccounts) != 1 || dry.NewAccounts[0].Login != "bobby" || dry.Imported["accounts"] != 1 {
		t.Fatalf("a dry run reports the account and writes nothing: users %d->%d %+v", users, afterDry, dry.NewAccounts)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.NewAccounts) != 1 || report.NewAccounts[0].V1Name != "bob" || report.NewAccounts[0].UserID == "" || report.Imported["accounts"] != 1 {
		t.Fatalf("report: %+v", report.NewAccounts)
	}
	id := report.NewAccounts[0].UserID
	// A real account that cannot sign in: a well-formed hash for a secret nobody holds.
	var login, hash, params string
	if err := s.db.QueryRow(`SELECT login,pw_hash,pw_params FROM user WHERE id=?`, id).Scan(&login, &hash, &params); err != nil || login != "bobby" {
		t.Fatalf("user: %q %v", login, err)
	}
	var stored struct {
		Algo       string `json:"algo"`
		Iterations int    `json:"iterations"`
		SaltLength int    `json:"salt_length"`
		Salt       string `json:"salt"`
	}
	raw, err := base64.RawStdEncoding.DecodeString(hash)
	if err != nil || len(raw) != 32 || json.Unmarshal([]byte(params), &stored) != nil || stored.Algo != "pbkdf2-hmac-sha256" || stored.SaltLength != 16 || stored.Iterations < 1 {
		t.Fatalf("the password is a random unusable hash in the sign-in format: %q %q %v", hash, params, err)
	}
	// Membership at the chosen role, in the project his rooms land in and no other; his messages are his.
	roles := map[string]string{}
	rows, _ := s.db.Query(`SELECT p.name,m.role FROM membership m JOIN project p ON p.id=m.project_id WHERE m.user_id=?`, id)
	for rows.Next() {
		var name, role string
		rows.Scan(&name, &role)
		roles[name] = role
	}
	rows.Close()
	if len(roles) != 1 || roles["proj"] != "operator" {
		t.Errorf("bob belongs to the project his room is in, as an operator: %v", roles)
	}
	var author string
	if err := s.db.QueryRow(`SELECT author_id FROM message WHERE seq=12`).Scan(&author); err != nil || author != id {
		t.Errorf("his message is attributed to the new account: %q want %q (%v)", author, id, err)
	}
	// A rerun finds the account and adds nothing.
	again, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || again.Imported["accounts"] != 0 || again.AlreadyPresent["accounts"] != 1 || len(again.NewAccounts) != 1 || again.NewAccounts[0].UserID != id {
		t.Errorf("rerun: %v %+v %+v", err, again.NewAccounts, again.AlreadyPresent)
	}
}

func TestV1ImportRefusesBadNewAccounts(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		change func(*V1ImportMap)
		want   string
	}{
		"bad login": {func(m *V1ImportMap) {
			m.NewUsers["bob"] = V1NewUser{Login: "Bob!", Role: "viewer"}
			m.Humans["bob"] = "Bob!"
		}, "the login must be 3 to 32"},
		"short login": {func(m *V1ImportMap) {
			m.NewUsers["bob"] = V1NewUser{Login: "bo", Role: "viewer"}
			m.Humans["bob"] = "bo"
		}, "the login must be 3 to 32"},
		"bad role":   {func(m *V1ImportMap) { m.NewUsers["bob"] = V1NewUser{Login: "bobby", Role: "owner"} }, "the role must be viewer or operator"},
		"not mapped": {func(m *V1ImportMap) { m.Humans["bob"] = "owner" }, "must be mapped to the new login"},
		"taken": {func(m *V1ImportMap) {
			m.NewUsers["bob"] = V1NewUser{Login: "owner", Role: "viewer"}
			m.Humans["bob"] = "owner"
		}, "already taken"},
		"an agent": {func(m *V1ImportMap) {
			m.NewUsers["bot@repo"] = V1NewUser{Login: "botty", Role: "viewer"}
			m.Humans["bot@repo"] = "botty"
		}, "only a person"},
		"two the same": {func(m *V1ImportMap) {
			m.NewUsers["alice"] = V1NewUser{Login: "bobby", Role: "viewer"}
			m.Humans["alice"] = "bobby"
		}, "would both be"},
	} {
		source, mapping := accountFixture(t)
		s := newV1RoleStore(t)
		c.change(&mapping)
		if _, err := s.ImportV1(context.Background(), source, mapping, true); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, c.want, err)
		}
	}
}

// v1's personal settings become the owner's preferences, an existing choice wins, and what v2 cannot take is listed.
func TestV1ImportCarriesPersonalSettingsToTheOwnersPreferences(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db := openV1Fixture(t, source)
	if _, err := db.Exec(`INSERT INTO settings(id,data) VALUES (1,'{"theme":"light","notification_volume":40,"notification_sound":"builtin:ding","show_system_events":false,"debug_button_enabled":true}')`); err != nil {
		t.Fatal(err)
	}
	s := newV1RoleStore(t)
	ctx := context.Background()
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.SkippedSettings) != 0 {
		t.Errorf("carried settings are not listed as skipped: %+v", preview.SkippedSettings)
	}
	// The owner already chose a theme in v2; that choice stays.
	if err := s.SetUserPreferences(ctx, "usr_owner", map[string]string{"theme": "dark"}); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatal(err)
	}
	prefs, err := s.UserPreferences(ctx, "usr_owner")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"theme": "dark", "inbox_volume": "40", "inbox_sound": "builtin:ding", "system_events": "0", "debug_inspector": "1"}
	if !reflect.DeepEqual(prefs, want) {
		t.Errorf("preferences: %v want %v", prefs, want)
	}
	if report.Imported["preferences"] != 4 || report.AlreadyPresent["preferences"] != 1 {
		t.Errorf("counts: %v %v", report.Imported, report.AlreadyPresent)
	}
}

func TestV1PersonalSettingsThatV2CannotTakeAreListed(t *testing.T) {
	t.Parallel()
	prefs, skipped := v1PersonalPreferences(map[string]json.RawMessage{
		"theme": json.RawMessage(`"neon"`), "notification_sound": json.RawMessage(`"user:4f3a"`), "notification_volume": json.RawMessage(`150`),
		"show_system_events": json.RawMessage(`"yes"`), "debug_button_enabled": json.RawMessage(`null`),
	})
	if len(prefs) != 0 {
		t.Errorf("nothing valid to carry: %v", prefs)
	}
	got := map[string]bool{}
	for _, entry := range skipped {
		got[entry.Key] = true
	}
	for _, key := range []string{"theme", "notification_sound", "notification_volume", "show_system_events"} {
		if !got[key] {
			t.Errorf("%s should be listed as skipped: %+v", key, skipped)
		}
	}
	if got["debug_button_enabled"] {
		t.Error("a null setting is simply absent")
	}
	// An empty sound is v1's default and carries nothing, without being an error.
	if p, s := v1PersonalPreferences(map[string]json.RawMessage{"notification_sound": json.RawMessage(`""`)}); len(p) != 0 || len(s) != 0 {
		t.Errorf("an empty sound: %v %v", p, s)
	}
}

// A person whose only v1 activity is a private chat still gets access to the chat's project, and the report says so out loud.
func TestV1ImportNamesAccessThatComesOnlyFromADirectMessage(t *testing.T) {
	t.Parallel()
	source, mapping := accountFixture(t)
	db := openV1Fixture(t, source)
	for _, id := range []string{"carol"} {
		raw, _ := json.Marshal(map[string]any{"id": id, "kind": "human", "name": id})
		if _, err := db.Exec(`INSERT INTO agents(id,data) VALUES (?,?)`, id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	room, _ := json.Marshal(map[string]any{"id": "dm:alice:carol", "members": []string{"alice", "carol"}, "created_at": "2026-10-01T12:00:00Z", "created_by": "alice"})
	if _, err := db.Exec(`INSERT INTO rooms(id,data) VALUES ('dm:alice:carol',?)`, string(room)); err != nil {
		t.Fatal(err)
	}
	msg, _ := json.Marshal(map[string]any{"id": 20, "room_id": "dm:alice:carol", "from": "carol", "from_kind": "human", "body": "hi", "created_at": "2026-10-01T12:00:00Z"})
	if _, err := db.Exec(`INSERT INTO messages(id,room_id,data) VALUES (20,'dm:alice:carol',?)`, string(msg)); err != nil {
		t.Fatal(err)
	}
	mapping.Rooms["dm:alice:carol"] = V1ImportRoom{Project: "proj", Lifecycle: "open"}
	mapping.Humans["carol"] = "carolly"
	mapping.NewUsers["carol"] = V1NewUser{Login: "carolly", Role: "viewer"}
	report, err := newV1RoleStore(t).ImportV1(context.Background(), source, mapping, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"carolly gets viewer access to project proj through a direct message"}
	if !reflect.DeepEqual(report.DMOnlyAccess, want) {
		t.Fatalf("only the person whose sole link is the chat is named: %q, want %q", report.DMOnlyAccess, want)
	}
	// Leaving the chat out leaves the person out of the project too.
	mapping.Rooms["dm:alice:carol"] = V1ImportRoom{Project: "proj", Lifecycle: "open", Skip: true}
	report, err = newV1RoleStore(t).ImportV1(context.Background(), source, mapping, true)
	if err != nil || len(report.DMOnlyAccess) != 0 {
		t.Fatalf("a skipped chat gives no access: %q %v", report.DMOnlyAccess, err)
	}
}
