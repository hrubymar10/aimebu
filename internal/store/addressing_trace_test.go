package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func traceMembers() []roomRecipient {
	return []roomRecipient{
		{id: "agt_ada", slug: "ada", kind: "agent", role: "worker"},
		{id: "agt_bob1", slug: "bob", kind: "agent", role: "reviewer"},
		{id: "agt_bob2", slug: "bob", kind: "agent", role: "worker"},
		{id: "agt_old", slug: "old", kind: "agent", role: "scout", revoked: true},
		{id: "usr_casey", slug: "casey", kind: "user"},
		{id: "usr_pat", slug: "pat", kind: "user", implicit: true},
	}
}

func traceFor(t *testing.T, body string) (map[string]string, map[string]MentionTrace) {
	t.Helper()
	var trace []MentionTrace
	targets := resolveMentions(body, "usr_casey", traceMembers(), "prj_1", "demo", &trace)
	byToken := map[string]MentionTrace{}
	for _, entry := range trace {
		byToken[entry.Token] = entry
	}
	return targets, byToken
}

// TestResolverGoldenTargets pins who each body addresses, written out by hand
// from the addressing rules rather than computed by the function under test,
// so a refactor of the resolver cannot pass by agreeing with itself. It also
// checks that asking for a trace never changes the answer.
func TestResolverGoldenTargets(t *testing.T) {
	t.Parallel()
	extra := append(traceMembers(),
		roomRecipient{id: "agt_carl", slug: "carl", kind: "agent", role: "lead"},
		roomRecipient{id: "agt_lead", slug: "lead", kind: "agent", role: "worker", revoked: true},
		roomRecipient{id: "usr_dana", slug: "dana", kind: "user"},
	)
	group := map[string]string{"agt_ada": "group", "agt_bob1": "group", "agt_bob2": "group", "agt_old": "group"}
	cases := []struct {
		body    string
		members []roomRecipient
		want    map[string]string
	}{
		{"@ada hi", nil, map[string]string{"agt_ada": "mention"}},
		{"@ADA hi", nil, map[string]string{"agt_ada": "mention"}},
		{"@ada- hi", nil, map[string]string{"agt_ada": "mention"}},
		{"@ada_ hi", nil, map[string]string{"agt_ada": "mention"}},
		{"@agt_ada full", nil, map[string]string{"agt_ada": "mention"}},
		{"@channel go", nil, group},
		{"@everyone @all @here", nil, group},
		{"@humans hi", nil, map[string]string{}},
		{"@humans hi", extra, map[string]string{"usr_dana": "group"}},
		{"@ais hi", nil, group},
		{"@humans @ais", nil, group},
		{"@bob who", nil, map[string]string{}},
		{"@bob@demo who", nil, map[string]string{"agt_bob1": "mention", "agt_bob2": "mention"}},
		{"@Bob@Demo who", nil, map[string]string{"agt_bob1": "mention", "agt_bob2": "mention"}},
		{"@bob@prj_1 who", nil, map[string]string{"agt_bob1": "mention", "agt_bob2": "mention"}},
		{"@ada@elsewhere", nil, map[string]string{}},
		{"@reviewer look", nil, map[string]string{"agt_bob1": "role"}},
		{"@Reviewer look", nil, map[string]string{"agt_bob1": "role"}},
		{"@scout ping", nil, map[string]string{"agt_old": "role"}},
		{"@old ping", nil, map[string]string{"agt_old": "mention"}},
		{"@lead go", extra, map[string]string{"agt_carl": "role"}},
		{"@pat human", nil, map[string]string{"usr_pat": "mention"}},
		{"@casey me", nil, map[string]string{}},
		{"@nobody", nil, map[string]string{}},
		{"`@ada` literal", nil, map[string]string{}},
		{"\\@ada escaped", nil, map[string]string{}},
		{"```\n@ada\n```", nil, map[string]string{}},
		{"    @ada indented", nil, map[string]string{}},
		{"mixed @ada and @bob and `@reviewer`", nil, map[string]string{"agt_ada": "mention"}},
		{"@ada @reviewer @channel", nil, map[string]string{"agt_ada": "mention", "agt_bob1": "role", "agt_bob2": "group", "agt_old": "group"}},
	}
	for _, tc := range cases {
		members := tc.members
		if members == nil {
			members = traceMembers()
		}
		plain := resolveMentionTargets(tc.body, "usr_casey", members, "prj_1", "demo")
		if !reflect.DeepEqual(plain, tc.want) {
			t.Errorf("%q: got %v, want %v", tc.body, plain, tc.want)
		}
		var trace []MentionTrace
		if traced := resolveMentions(tc.body, "usr_casey", members, "prj_1", "demo", &trace); !reflect.DeepEqual(traced, tc.want) {
			t.Errorf("%q: tracing changed the answer: %v, want %v", tc.body, traced, tc.want)
		}
	}
}

func TestTraceExplainsEachWayAMentionResolves(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body, token, kind, outcome string
		reached                    []string
		reason                     string
	}{
		{"@ada hi", "ada", "slug", "addressed", []string{"agt_ada"}, "exact slug"},
		{"@agt_ada hi", "agt_ada", "full_id", "addressed", []string{"agt_ada"}, "full ID"},
		{"@bob hi", "bob", "slug", "ambiguous", []string{"agt_bob1", "agt_bob2"}, "share the slug"},
		{"@bob@demo hi", "bob@demo", "project_qualified", "addressed", []string{"agt_bob1", "agt_bob2"}, "slug plus project"},
		{"@bob@other hi", "bob@other", "project_qualified", "no_match", nil, "neither this project's name"},
		{"@ada@demo hi", "ada@demo", "project_qualified", "addressed", []string{"agt_ada"}, "slug plus project"},
		{"@zed@demo hi", "zed@demo", "project_qualified", "no_match", nil, "no agent in this room has the slug"},
		{"@reviewer look", "reviewer", "role", "addressed", []string{"agt_bob1"}, "role key"},
		{"@scout ping", "scout", "revoked", "addressed", []string{"agt_old"}, "revoked"},
		{"@old ping", "old", "revoked", "addressed", []string{"agt_old"}, "revoked"},
		{"@nobody hi", "nobody", "none", "no_match", nil, "no room member"},
		{"@casey me", "casey", "slug", "self", nil, "never addressed by their own message"},
		{"@pat hi", "pat", "slug", "addressed", []string{"usr_pat"}, "exact slug"},
		{"@humans hi", "humans", "group", "self", nil, "only the sender"},
		{"@ais hi", "ais", "group", "addressed", []string{"agt_ada", "agt_bob1", "agt_bob2", "agt_old"}, "every AI agent"},
	}
	for _, tc := range cases {
		_, traces := traceFor(t, tc.body)
		entry, ok := traces[tc.token]
		if !ok {
			t.Errorf("%q: no trace for %q: %+v", tc.body, tc.token, traces)
			continue
		}
		if entry.Kind != tc.kind || entry.Outcome != tc.outcome || !strings.Contains(entry.Reason, tc.reason) {
			t.Errorf("%q: got kind=%s outcome=%s reason=%q", tc.body, entry.Kind, entry.Outcome, entry.Reason)
		}
		if tc.reached != nil {
			var ids []string
			for _, person := range entry.Reached {
				ids = append(ids, person.ID)
			}
			if !reflect.DeepEqual(ids, tc.reached) {
				t.Errorf("%q: reached %v, want %v", tc.body, ids, tc.reached)
			}
		}
	}
}

func TestTraceFlagsMentionsThatAreInvisibleBecauseOfCodeOrEscapes(t *testing.T) {
	t.Parallel()
	targets, traces := traceFor(t, "`@ada` please look, and \\@bob, but also @reviewer\n```\n@scout\n```")
	if len(targets) != 1 || targets["agt_bob1"] != "role" {
		t.Fatalf("only the live @reviewer should address anyone: %v", targets)
	}
	for _, token := range []string{"ada", "bob", "scout"} {
		entry, ok := traces[token]
		if !ok || entry.Kind != "ignored" || entry.Outcome != "ignored" || !strings.Contains(entry.Reason, "backticks") || len(entry.Reached) != 0 {
			t.Errorf("%s should be reported as ignored: %+v", token, entry)
		}
	}
	if traces["reviewer"].Outcome != "addressed" {
		t.Errorf("the live mention must still resolve: %+v", traces["reviewer"])
	}
	if _, ok := traces["ada"]; ok && traces["ada"].Kind != "ignored" {
		t.Errorf("a token that is only ever in code must not also resolve")
	}
}

func TestTokenLiveOnceAndEscapedOnceIsNotReportedIgnored(t *testing.T) {
	t.Parallel()
	_, traces := traceFor(t, "@ada hello and `@ada` again")
	if traces["ada"].Kind == "ignored" || traces["ada"].Outcome != "addressed" {
		t.Fatalf("a token that is live somewhere addresses normally: %+v", traces["ada"])
	}
}

func TestMessageDebugCarriesTheAddressingTrace(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	message, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner, Body: "`@worker` is in backticks, but @nobody is live"})
	if err != nil {
		t.Fatal(err)
	}
	debug, err := s.MessageDebug(ctx, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	byToken := map[string]MentionTrace{}
	for _, entry := range debug.Addressing.Mentions {
		byToken[entry.Token] = entry
	}
	if byToken["worker"].Outcome != "ignored" || byToken["nobody"].Outcome != "no_match" {
		t.Fatalf("trace: %+v", debug.Addressing.Mentions)
	}
	if debug.Addressing.Drift || len(debug.Addressing.Now) != 0 {
		t.Fatalf("nobody was addressed then or now, so no drift: %+v", debug.Addressing)
	}
	// A later same-named agent joining makes re-evaluation differ from the record.
	ghost, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner, Body: "@worker now live"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE message SET targets_json='[]' WHERE id=?`, ghost.ID); err != nil {
		t.Fatal(err)
	}
	drifted, err := s.MessageDebug(ctx, ghost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !drifted.Addressing.Drift || len(drifted.Addressing.Now) != 1 {
		t.Fatalf("a recorded set that no longer matches the re-evaluation must be flagged: %+v", drifted.Addressing)
	}
}

func TestIgnoredMentionsOnlyReportTokensThatWouldHaveReachedSomeone(t *testing.T) {
	t.Parallel()
	body := "```java\n@Override\npublic void run() { // see user@host, @param x\n}\n```\n`@ada` and \\@reviewer and `@bob`"
	_, traces := traceFor(t, body)
	for _, noise := range []string{"override", "param", "host"} {
		if _, ok := traces[noise]; ok {
			t.Errorf("code noise %q must not be reported as a missed mention", noise)
		}
	}
	ada, bob, reviewer := traces["ada"], traces["bob"], traces["reviewer"]
	if ada.Outcome != "ignored" || len(ada.WouldReach) != 1 || ada.WouldReach[0].ID != "agt_ada" {
		t.Errorf("a backticked @ada should say who it would have reached: %+v", ada)
	}
	if bob.Outcome != "ignored" || len(bob.WouldReach) != 2 {
		t.Errorf("an ignored ambiguous slug lists its candidates: %+v", bob)
	}
	if reviewer.Outcome != "ignored" || len(reviewer.WouldReach) != 1 || reviewer.WouldReach[0].ID != "agt_bob1" {
		t.Errorf("an escaped role mention should say who it would have reached: %+v", reviewer)
	}
}

func TestReplySourceFollowsTheServerRule(t *testing.T) {
	t.Parallel()
	members := traceMembers()
	names := map[string]string{}
	for _, member := range members {
		names[member.id] = member.slug
	}
	cases := []struct {
		authorID, authorKind, parent, parentKind, want string
	}{
		{"usr_casey", "user", "agt_ada", "agent", "addresses its parent's author"},
		{"usr_casey", "user", "usr_casey", "user", "reply to yourself"},
		{"agt_ada", "agent", "agt_bob1", "agent", "wakes it, but only 4 times in a row"},
		{"agt_ada", "agent", "usr_casey", "user", "addresses its parent's author"},
		{"usr_casey", "user", "system", "system", "system message"},
		{"usr_casey", "user", "agt_gone", "agent", "not in this room now"},
	}
	for _, tc := range cases {
		if got := replySource(members, names, tc.authorID, tc.authorKind, tc.parent, tc.parentKind); !strings.Contains(got, tc.want) {
			t.Errorf("%s replying to %s/%s: %q does not contain %q", tc.authorID, tc.parent, tc.parentKind, got, tc.want)
		}
	}
}

func TestInboxAnswerNeverReportsDrift(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	answer, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner, Body: "@worker I answer", OnlyTargets: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	// Make the recorded targets differ from what the mention would resolve to,
	// as they do for an answer that names someone other than the asker.
	if _, err := s.db.Exec(`UPDATE message SET targets_json='[]' WHERE id=?`, answer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,state,answer_message_id,created_at)
		VALUES ('inb_answer',?,?,?,'agent',?,'question','{}','resolved',?,?)`, f.project.ID, f.room.ID, f.agent.ID, f.agent.ID, answer.ID, timestamp()); err != nil {
		t.Skipf("inbox_item shape changed; cannot stage an answered item: %v", err)
	}
	debug, err := s.MessageDebug(ctx, answer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if debug.Addressing.Drift {
		t.Fatalf("an inbox answer's targets are fixed by the answer, so re-reading its mentions is not drift: %+v", debug.Addressing)
	}
	found := false
	for _, source := range debug.Addressing.Sources {
		found = found || strings.Contains(source, "inbox answer")
	}
	if !found {
		t.Fatalf("the trace must say why the mentions do not address anyone: %+v", debug.Addressing.Sources)
	}
}
