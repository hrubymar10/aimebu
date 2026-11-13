package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func role(id, key, extends, body string) RoleDefinition {
	return RoleDefinition{ID: id, Key: key, Extends: extends, Body: body}
}

func TestResolveRoleBody(t *testing.T) {
	t.Parallel()
	all := []RoleDefinition{
		role("g_base", "base", "", "  base text  \n"),
		role("g_mid", "mid", "base", "mid text"),
		role("g_top", "top", "mid", "top text"),
		role("g_alone", "alone", "", "  alone, untouched \n"),
		role("g_dangling", "dangling", "ghost", "own body"),
		role("g_a", "cyc-a", "cyc-b", "a"),
		role("g_b", "cyc-b", "cyc-a", "b"),
		role("g_empty", "emptybody", "base", "  "),
	}
	byID := func(id string) RoleDefinition {
		for _, r := range all {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("no role %s", id)
		return RoleDefinition{}
	}
	for _, c := range []struct{ id, want, why string }{
		{"g_alone", "  alone, untouched \n", "a role with no parent gives its body untouched"},
		{"g_mid", "base text\n\nmid text", "parent, a blank line, own body, each trimmed"},
		{"g_top", "base text\n\nmid text\n\ntop text", "a chain resolves root first"},
		{"g_dangling", "own body", "a missing parent leaves the role's own body"},
		{"g_a", "b\n\na", "a cycle that slipped in stops instead of looping"},
		{"g_empty", "base text", "an empty own body adds nothing"},
	} {
		if got := resolveRoleBody(byID(c.id), all); got != c.want {
			t.Errorf("%s: %s: got %q, want %q", c.id, c.why, got, c.want)
		}
	}
	// The chain is cut at maxRoleExtendsDepth ancestors.
	var deep []RoleDefinition
	for i := 0; i <= maxRoleExtendsDepth+2; i++ {
		r := role("d"+string(rune('a'+i)), "d"+string(rune('a'+i)), "", string(rune('A'+i)))
		if i > 0 {
			r.Extends = "d" + string(rune('a'+i-1))
		}
		deep = append(deep, r)
	}
	top := deep[len(deep)-1]
	if got := strings.Count(resolveRoleBody(top, deep), "\n\n") + 1; got != maxRoleExtendsDepth+1 {
		t.Errorf("a deep chain resolved %d bodies, want %d", got, maxRoleExtendsDepth+1)
	}
}

func TestCheckRoleExtends(t *testing.T) {
	t.Parallel()
	all := []RoleDefinition{
		role("g_reviewer", "reviewer", "", "r"),
		role("g_a", "alpha", "reviewer", "a"),
		role("g_b", "beta", "alpha", "b"),
	}
	candidate := func(id, key, extends string) RoleDefinition { return role(id, key, extends, "x") }
	for _, c := range []struct {
		name string
		role RoleDefinition
		ok   bool
	}{
		{"no parent", candidate("", "new", ""), true},
		{"existing parent", candidate("", "new", "reviewer"), true},
		{"a chain", candidate("", "new", "beta"), true},
		{"unknown target", candidate("", "new", "ghost"), false},
		{"malformed key", candidate("", "new", "Not A Key"), false},
		{"itself, new", candidate("", "reviewer2", "reviewer2"), false},
		{"itself, existing", candidate("g_a", "alpha", "alpha"), false},
		{"a cycle through existing roles", candidate("g_reviewer", "reviewer", "beta"), false},
		{"a direct cycle", candidate("g_reviewer", "reviewer", "alpha"), false},
	} {
		list := append([]RoleDefinition{}, all...)
		if err := checkRoleExtends(c.role, list); (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
	// Six ancestors is one too many.
	var chain []RoleDefinition
	for i := 0; i < maxRoleExtendsDepth+1; i++ {
		r := role("c"+string(rune('a'+i)), "c"+string(rune('a'+i)), "", "x")
		if i > 0 {
			r.Extends = "c" + string(rune('a'+i-1))
		}
		chain = append(chain, r)
	}
	if err := checkRoleExtends(role("", "toodeep", chain[len(chain)-1].Key, "x"), chain); !errors.Is(err, ErrRoleExtends) {
		t.Errorf("a chain of %d ancestors: %v", maxRoleExtendsDepth+1, err)
	}
	if err := checkRoleExtends(role("", "justfits", chain[len(chain)-2].Key, "x"), chain); err != nil {
		t.Errorf("a chain of %d ancestors: %v", maxRoleExtendsDepth, err)
	}
}

func globalRoles(t *testing.T, s *Store) map[string]RoleDefinition {
	t.Helper()
	roles, err := s.ListRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]RoleDefinition{}
	for _, r := range roles {
		byKey[r.Key] = r
	}
	return byKey
}

func TestBuiltinRolesMatchTheDatabase(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	got := globalRoles(t, s)
	want := builtinRoles()
	if len(got) != len(want) {
		t.Fatalf("fresh database has %d global roles, the defaults table %d", len(got), len(want))
	}
	for id, def := range want {
		r, ok := got[def.Key]
		if !ok || r.ID != id {
			t.Fatalf("role %s missing or has another id: %+v", id, r)
		}
		if r.Label != def.Label || r.Emoji != def.Emoji || r.Body != def.Body || r.Extends != def.Extends || r.Cardinality != def.Cardinality ||
			r.Lead != def.Lead || r.CanSpawn != def.CanSpawn || r.Writer != def.Writer {
			t.Errorf("%s: the shipped role and the defaults table differ:\n db %+v\n def %+v", id, r, def)
		}
	}
	if strings.TrimSpace(reviewerRoleBody) != reviewerRoleBody {
		t.Fatal("the reviewer body has outer whitespace, so a specialist's resolved text would no longer equal the old flattened text")
	}
	// The specialists carry only their focus, and what an agent receives is what the flattened copy used to say.
	for _, focus := range reviewerFocus {
		r := got[focus.key]
		if r.Extends != "reviewer" || !strings.HasPrefix(r.Body, "Additional focus: ") || strings.Contains(r.Body, "ordered by severity") {
			t.Errorf("%s should hold only its focus and extend reviewer: extends=%q body=%.60q", focus.key, r.Extends, r.Body)
		}
		if want := currentReviewerRoleBody + "\n\nAdditional focus: " + focus.body; r.ResolvedBody != want {
			t.Errorf("%s resolves to something other than the old flattened text", focus.key)
		}
	}
	if got["reviewer"].ResolvedBody != currentReviewerRoleBody || got["worker"].ResolvedBody != currentWorkerRoleBody {
		t.Error("a role without a parent resolves to its own body")
	}
}

func TestRoleExtendsMigrationKeepsEditedSpecialists(t *testing.T) {
	db, path := roleDatabaseBefore(t, 54)
	// ux: an edited body; sec: the shipped body but an edited name; test: untouched. Edits made before the migration bump the revision.
	for _, statement := range []string{
		`UPDATE role SET body='owner edit',rev=rev+1 WHERE id='role_ux-reviewer_default'`,
		`UPDATE role SET label='Security folk',rev=rev+1 WHERE id='role_sec-reviewer_default'`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := globalRoles(t, s)
	if ux := got["ux-reviewer"]; ux.Body != "owner edit" || ux.Extends != "" || ux.Rev != 1 || ux.ResolvedBody != "owner edit" {
		t.Errorf("a specialist with an edited body must stay as the owner left it: %+v", ux)
	}
	wantSec := reviewerRoleBody + "\n\nAdditional focus: " + reviewerFocus[0].body
	if sec := got["sec-reviewer"]; sec.Label != "Security folk" || sec.Extends != "" || sec.Rev != 1 || sec.Body != wantSec {
		t.Errorf("a specialist with an edited name counts as edited and stays complete: %+v", sec)
	}
	for _, focus := range reviewerFocus {
		if focus.key != "test-reviewer" {
			continue
		}
		r := got[focus.key]
		if r.Extends != "reviewer" || r.Rev != 0 || r.Body != specialistFocus(focus.body) || r.ResolvedBody != currentReviewerRoleBody+"\n\nAdditional focus: "+focus.body {
			t.Errorf("%s: unedited specialist not converted cleanly: extends=%q rev=%d", focus.key, r.Extends, r.Rev)
		}
	}
	// Running it again changes nothing, and the column step tolerates a database that already has the column.
	before := globalRoles(t, s)
	if _, err := s.db.Exec(roleExtendsMigration()); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if err := s.Tx(context.Background(), func(tx *Tx) error { return ensureRoleExtendsColumn(tx.ctx, tx.tx) }); err != nil {
		t.Fatalf("ensure column again: %v", err)
	}
	after := globalRoles(t, s)
	for key, r := range before {
		if a := after[key]; a.Body != r.Body || a.Extends != r.Extends || a.Rev != r.Rev {
			t.Errorf("%s changed on a rerun: %+v -> %+v", key, r, a)
		}
	}
}

func TestManageRoleExtends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	project, _, _ := fixture(t, s)
	roleReviewUsers(t, s, project.ID)
	base := func(key, extends, body string) RoleDefinition {
		return RoleDefinition{Key: key, Label: key, Body: body, Extends: extends, Cardinality: "multi"}
	}
	qa, err := s.ManageRole(ctx, "usr_owner", base("qa", "reviewer", "qa focus"), false)
	if err != nil {
		t.Fatalf("a role extending reviewer: %v", err)
	}
	if got := globalRoles(t, s)["qa"].ResolvedBody; got != currentReviewerRoleBody+"\n\nqa focus" {
		t.Errorf("qa resolves to %.80q", got)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", base("lost", "ghost", "x"), false); !errors.Is(err, ErrRoleExtends) {
		t.Errorf("an unknown parent: %v", err)
	}
	selfRef := qa
	selfRef.Extends = "qa"
	if _, err := s.ManageRole(ctx, "usr_owner", selfRef, false); !errors.Is(err, ErrRoleExtends) {
		t.Errorf("a role extending itself: %v", err)
	}
	one, err := s.ManageRole(ctx, "usr_owner", base("one", "qa", "one"), false)
	if err != nil {
		t.Fatal(err)
	}
	qa.Extends = "one"
	if _, err := s.ManageRole(ctx, "usr_owner", qa, false); !errors.Is(err, ErrRoleExtends) {
		t.Errorf("a cycle qa -> one -> qa: %v", err)
	}
	// A role others extend cannot be deleted; once they stop, it can.
	if _, err := s.ManageRole(ctx, "usr_owner", qa, true); !errors.Is(err, ErrRoleExtended) {
		t.Errorf("deleting an extended role: %v", err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", one, true); err != nil {
		t.Fatalf("deleting the child: %v", err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", qa, true); err != nil {
		t.Fatalf("deleting qa once nothing extends it: %v", err)
	}
	// A role that extends the reviewer resolves to the sum, for the dispatcher too; a deleted role answers with its own stored body.
	live, err := s.ManageRole(ctx, "usr_owner", base("sum", "reviewer", "sum rules"), false)
	if err != nil {
		t.Fatalf("a role extending reviewer: %v", err)
	}
	var dispatched, summed, orphan string
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		if dispatched, err = tx.effectiveRoleBody("role_sec-reviewer_default"); err != nil {
			return err
		}
		summed, err = tx.effectiveRoleBody(live.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", live, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		orphan, err = tx.effectiveRoleBody(live.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dispatched, currentReviewerRoleBody+"\n\nAdditional focus: ") {
		t.Errorf("the dispatcher would send %.80q", dispatched)
	}
	if summed != currentReviewerRoleBody+"\n\nsum rules" {
		t.Errorf("the role reaches the dispatcher as %.80q", summed)
	}
	if orphan != "sum rules" {
		t.Errorf("a deleted role reaches the dispatcher as %.80q", orphan)
	}
}

func TestResetRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	project, _, _ := fixture(t, s)
	roleReviewUsers(t, s, project.ID)
	sec := globalRoles(t, s)["sec-reviewer"]
	def := builtinRoles()["role_sec-reviewer_default"]
	edited := sec
	edited.Label, edited.Emoji, edited.Body, edited.Extends, edited.Writer = "Mine", "X", "all new", "", false
	edited, err := s.ManageRole(ctx, "usr_owner", edited, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetRole(ctx, "usr_operator", sec.ID, edited.Rev); !errors.Is(err, ErrForbidden) {
		t.Errorf("an operator resetting a global role: %v", err)
	}
	if _, err := s.ResetRole(ctx, "usr_owner", sec.ID, edited.Rev+7); !errors.Is(err, ErrStaleRev) {
		t.Errorf("a stale revision: %v", err)
	}
	custom, err := s.ManageRole(ctx, "usr_owner", RoleDefinition{Key: "custom", Label: "Custom", Body: "c", Cardinality: "multi"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetRole(ctx, "usr_owner", custom.ID, custom.Rev); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("resetting a role that is not built in: %v", err)
	}
	reset, err := s.ResetRole(ctx, "usr_owner", sec.ID, edited.Rev)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset.Label != def.Label || reset.Emoji != def.Emoji || reset.Body != def.Body || reset.Extends != "reviewer" || reset.Rev != edited.Rev+1 ||
		reset.ResolvedBody != currentReviewerRoleBody+"\n\n"+def.Body {
		t.Errorf("reset gave %+v", reset)
	}
	if got := globalRoles(t, s)["sec-reviewer"]; got.Body != def.Body || got.Rev != reset.Rev {
		t.Errorf("the stored role after reset: %+v", got)
	}
	// Resetting must not create a cycle: reviewer extends sec-reviewer now, so sec-reviewer cannot get its parent back.
	reviewer := globalRoles(t, s)["reviewer"]
	reviewer.Extends = "sec-reviewer"
	if _, err := s.ManageRole(ctx, "usr_owner", reviewer, false); !errors.Is(err, ErrRoleExtends) {
		t.Fatalf("reviewer extending a role that extends it: %v", err)
	}
	// Edit sec-reviewer to stand alone, make reviewer extend it, then a reset would close the loop.
	standalone := globalRoles(t, s)["sec-reviewer"]
	standalone.Extends = ""
	standalone, err = s.ManageRole(ctx, "usr_owner", standalone, false)
	if err != nil {
		t.Fatal(err)
	}
	reviewer = globalRoles(t, s)["reviewer"]
	reviewer.Extends = "sec-reviewer"
	if _, err := s.ManageRole(ctx, "usr_owner", reviewer, false); err != nil {
		t.Fatalf("reviewer extending a standalone role: %v", err)
	}
	if _, err := s.ResetRole(ctx, "usr_owner", standalone.ID, standalone.Rev); !errors.Is(err, ErrRoleExtends) {
		t.Errorf("a reset that would make reviewer and sec-reviewer extend each other: %v", err)
	}
	// Capabilities and cardinality come back too (here the lead, edited to a multi role with no capabilities).
	lead := globalRoles(t, s)["leader"]
	lead.Cardinality, lead.Lead, lead.CanSpawn = "multi", false, false
	lead, err = s.ManageRole(ctx, "usr_owner", lead, false)
	if err != nil {
		t.Fatal(err)
	}
	backLead, err := s.ResetRole(ctx, "usr_owner", lead.ID, lead.Rev)
	if err != nil {
		t.Fatal(err)
	}
	if backLead.Cardinality != "singleton" || !backLead.Lead || !backLead.CanSpawn || backLead.Writer {
		t.Errorf("the lead after reset: %+v", backLead)
	}
}

// Changing a role must not leave a role further down its chain with more ancestors than resolution reads: its resolved body would
// silently lose the top of the chain.
func TestRoleExtendsDepthChecksDescendants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	project, _, _ := fixture(t, s)
	roleReviewUsers(t, s, project.ID)
	add := func(key, parent string) RoleDefinition {
		r, err := s.ManageRole(ctx, "usr_owner", RoleDefinition{Key: key, Label: key, Body: key, Extends: parent, Cardinality: "multi"}, false)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return r
	}
	chain := func(prefix string, length int) RoleDefinition {
		root := add(prefix+"0", "")
		for i := 1; i < length; i++ {
			add(prefix+string(rune('0'+i)), prefix+string(rune('0'+i-1)))
		}
		return root
	}
	topper := add("topper", "")
	// chain5 has five ancestors, which is allowed; giving its root a parent would make six.
	root := chain("chain", maxRoleExtendsDepth+1)
	root.Extends = topper.Key
	if _, err := s.ManageRole(ctx, "usr_owner", root, false); !errors.Is(err, ErrRoleExtends) {
		t.Fatalf("extending the root of a five-ancestor chain would give its last role six ancestors: %v", err)
	}
	// A shorter chain may grow by one.
	short := chain("short", 3)
	short.Extends = topper.Key
	if _, err := s.ManageRole(ctx, "usr_owner", short, false); err != nil {
		t.Fatalf("a short chain may grow: %v", err)
	}
	// A role below a chain of five counts too: it has five ancestors, so the root cannot take a parent.
	deepRoot := chain("deep", maxRoleExtendsDepth)
	add("below", "deep"+string(rune('0'+maxRoleExtendsDepth-1)))
	deepRoot.Extends = topper.Key
	if _, err := s.ManageRole(ctx, "usr_owner", deepRoot, false); !errors.Is(err, ErrRoleExtends) {
		t.Fatalf("a role below the chain deepens it too: %v", err)
	}
}

// A cycle that does not pass through the role being checked must still end the walk (A -> B -> C -> B).
func TestRoleWalksStopOnACycleBesideTheStart(t *testing.T) {
	t.Parallel()
	list := []RoleDefinition{
		{ID: "a", Key: "ra", Extends: "rb"},
		{ID: "b", Key: "rb", Extends: "rc"},
		{ID: "c", Key: "rc", Extends: "rb"},
	}
	if err := checkRoleExtends(list[0], list); !errors.Is(err, ErrRoleExtends) {
		t.Errorf("checkRoleExtends on a cycle below the start: %v", err)
	}
	if err := checkChainDepths(list, ""); !errors.Is(err, ErrRoleExtends) {
		t.Errorf("checkChainDepths on a cycle: %v", err)
	}
}
