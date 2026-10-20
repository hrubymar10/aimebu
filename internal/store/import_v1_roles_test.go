package store

import (
	"strings"
	"testing"
)

func TestResolveV1RolesKeepsParentLinks(t *testing.T) {
	t.Parallel()
	destination := map[string]RoleDefinition{
		"reviewer":     {Key: "reviewer", Body: "reviewer text", Cardinality: "multi"},
		"sec-reviewer": {Key: "sec-reviewer", Body: "Additional focus: sec", Extends: "reviewer", Cardinality: "multi"},
	}
	source := map[string]v1ImportRole{
		"deep-reviewer": {Key: "deep-reviewer", Body: " Extra checks ", Extends: "reviewer", Custom: true},
		"sec-reviewer":  {Key: "sec-reviewer", Body: "v1 focus"},
		"pair-a":        {Key: "pair-a", Body: "A", Extends: "pair-b", Custom: true},
		"pair-b":        {Key: "pair-b", Body: "B", Custom: true},
		"orphan":        {Key: "orphan", Body: "O", Extends: "ghost", Custom: true},
		"selfish":       {Key: "selfish", Body: "S", Extends: "selfish", Custom: true},
		"loop-a":        {Key: "loop-a", Body: "a", Extends: "loop-b", Custom: true},
		"loop-b":        {Key: "loop-b", Body: "b", Extends: "loop-a", Custom: true},
		"plain":         {Key: "plain", Body: "plain body", Custom: true},
	}
	got := map[string]V1PreviewRole{}
	for _, role := range resolveV1Roles(source, destination, nil) {
		got[role.Key] = role
	}
	linked := func(key, parent, body string) {
		t.Helper()
		r := got[key]
		if r.Extends != parent || r.FlattenedFrom != "" || r.Body != body || r.SkippedReason != "" {
			t.Errorf("%s: want a link to %q with body %q, got %+v", key, parent, body, r)
		}
	}
	linked("deep-reviewer", "reviewer", "Extra checks")
	linked("sec-reviewer", "reviewer", "v1 focus")
	linked("pair-a", "pair-b", "A")
	linked("plain", "", "plain body")
	for _, c := range []struct{ key, from, reason string }{
		{"orphan", "ghost", "missing parent"},
		{"selfish", "selfish", "cycle"},
		{"loop-a", "", "cycle"},
	} {
		r := got[c.key]
		// A role whose parent is unavailable keeps no link; a loop between roles that exist is skipped whatever its link says.
		if (c.from != "" && (r.Extends != "" || r.FlattenedFrom != c.from)) || !strings.Contains(r.SkippedReason, c.reason) {
			t.Errorf("%s: a parent that is unavailable or loops is skipped with a reason: %+v", c.key, r)
		}
	}

	// A parent that v2 will not import (here: a description over v2's 80-character limit) cannot be linked to, so its child is
	// flattened from the v1 text instead, as before, and the import does not fail on a link to nothing.
	source2 := map[string]v1ImportRole{
		"base":      {Key: "base", Body: "base text", Description: strings.Repeat("d", 90), Custom: true},
		"child":     {Key: "child", Body: "child text", Extends: "base", Custom: true},
		"bad-card":  {Key: "bad-card", Body: "bc", Cardinality: "triple", Custom: true},
		"under-bad": {Key: "under-bad", Body: "ub", Extends: "bad-card", Custom: true},
		"Bad Key":   {Key: "Bad Key", Body: "bk", Custom: true},
		"under-key": {Key: "under-key", Body: "uk", Extends: "Bad Key", Custom: true},
		"ok-parent": {Key: "ok-parent", Body: "op", Custom: true},
		"ok-child":  {Key: "ok-child", Body: "oc", Extends: "ok-parent", Custom: true},
	}
	got2 := map[string]V1PreviewRole{}
	for _, role := range resolveV1Roles(source2, destination, nil) {
		got2[role.Key] = role
	}
	if r := got2["base"]; !strings.Contains(r.SkippedReason, "limits") {
		t.Errorf("base should be ruled out by its description: %+v", r)
	}
	for _, child := range []struct{ key, parent, body string }{{"child", "base", "base text\n\nchild text"}, {"under-bad", "bad-card", "bc\n\nub"}, {"under-key", "Bad Key", "bk\n\nuk"}} {
		if r := got2[child.key]; r.Extends != "" || r.FlattenedFrom != child.parent || r.Body != child.body || r.SkippedReason != "" {
			t.Errorf("%s under a skipped parent should be flattened and imported: %+v", child.key, r)
		}
	}
	if r := got2["ok-child"]; r.Extends != "ok-parent" || r.Body != "oc" {
		t.Errorf("a child of a parent that will be imported keeps its link: %+v", r)
	}
}
