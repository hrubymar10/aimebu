package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/types"
)

func TestRegisterSameSlugDifferentProjects(t *testing.T) {
	s, _ := setupTestServer(t)

	a, reclaimed, err := s.registerAI("gpt5", "codex", "alpha", nil, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed {
		t.Fatal("first registration unexpectedly reclaimed")
	}
	b, reclaimed, err := s.registerAI("gpt5", "codex", "beta", nil, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed {
		t.Fatal("second project registration unexpectedly reclaimed")
	}

	if a.ID != "sam@alpha" {
		t.Fatalf("first id = %q, want sam@alpha", a.ID)
	}
	if b.ID != "sam@beta" {
		t.Fatalf("second id = %q, want sam@beta", b.ID)
	}
}

func TestForceClaimSameSlugPerProject(t *testing.T) {
	s, _ := setupTestServer(t)

	if _, _, err := s.registerAI("gpt5", "codex", "alpha", nil, "sam"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.registerAI("opus4.7", "claude-code", "beta", nil, "sam"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.registerAI("opus4.7", "claude-code", "alpha", nil, "sam"); err == nil {
		t.Fatal("same slug in same project with different model/harness should conflict")
	}
}

func TestAmbiguousMentionInRoomWarnsAndDoesNotResolve(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "gamma", nil, "sndr")
	if err != nil {
		t.Fatal(err)
	}
	alpha, _, err := s.registerAI("gpt5", "codex", "alpha", nil, "sam")
	if err != nil {
		t.Fatal(err)
	}
	beta, _, err := s.registerAI("gpt5", "codex", "beta", nil, "sam")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{sender.ID, alpha.ID, beta.ID} {
		if _, err := s.joinRoom("general", id); err != nil {
			t.Fatal(err)
		}
	}

	resp := postRoomMessage(t, srv.URL, "general", sender.ID, "@sam check this")
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "@sam is ambiguous") {
		t.Fatalf("warnings = %v, want one ambiguous @sam warning", resp.Warnings)
	}

	msg, ok := s.messageByID(resp.ID)
	if !ok {
		t.Fatalf("message %d not found", resp.ID)
	}
	if len(msg.Targets) != 0 {
		t.Fatalf("targets = %v, want no ambiguous slug target", msg.Targets)
	}
}

func TestDisambiguatedMentionResolvesFullName(t *testing.T) {
	s, _ := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "gamma", nil, "sndr")
	if err != nil {
		t.Fatal(err)
	}
	alpha, _, err := s.registerAI("gpt5", "codex", "alpha", nil, "sam")
	if err != nil {
		t.Fatal(err)
	}
	beta, _, err := s.registerAI("gpt5", "codex", "beta", nil, "sam")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{sender.ID, alpha.ID, beta.ID} {
		if _, err := s.joinRoom("general", id); err != nil {
			t.Fatal(err)
		}
	}

	id, err := s.roomSend("general", sender.ID, "@sam@alpha check this", false, nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := s.messageByID(id)
	if !ok {
		t.Fatalf("message %d not found", id)
	}
	if len(msg.Targets) != 1 || msg.Targets[0] != "sam@alpha" {
		t.Fatalf("targets = %v, want [sam@alpha]", msg.Targets)
	}

	alphaView := annotate([]types.Message{msg}, alpha.ID, s.addressingContext)
	if len(alphaView) != 1 || !alphaView[0].AddressedToMe || !alphaView[0].ShouldRespond {
		t.Fatalf("alpha annotated view = %+v, want addressed responder", alphaView)
	}
	betaView := annotate([]types.Message{msg}, beta.ID, s.addressingContext)
	if len(betaView) != 1 || betaView[0].AddressedToMe || betaView[0].ShouldRespond {
		t.Fatalf("beta annotated view = %+v, want not addressed", betaView)
	}
}

func TestSingletonRoleAcrossProjectsDifferentRooms(t *testing.T) {
	s, _ := setupTestServer(t)

	alpha, _, err := s.registerAI("gpt5", "codex", "alpha", nil, "sam")
	if err != nil {
		t.Fatal(err)
	}
	beta, _, err := s.registerAI("gpt5", "codex", "beta", nil, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("alpha-room", alpha.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("beta-room", beta.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.assignRole("alpha-room", alpha.ID, "leader"); err != nil {
		t.Fatal(err)
	}
	if err := s.assignRole("beta-room", beta.ID, "leader"); err != nil {
		t.Fatal(err)
	}
}

func TestSlugPatternValid(t *testing.T) {
	valid := []string{
		"alice",         // plain alpha
		"foo-bar",       // hyphen mid-name
		"foo_bar",       // underscore mid-name
		"a1b",           // digit mid-name, length 3
		"ab1",           // min length 3
		"a-b",           // hyphen at position 1
		"abcdefghijklmnopqrstu", // max length 21
	}
	for _, s := range valid {
		if !SlugPattern.MatchString(s) {
			t.Errorf("SlugPattern rejected valid slug %q", s)
		}
	}
}

func TestSlugPatternInvalid(t *testing.T) {
	invalid := []string{
		"",                       // empty
		"ab",                     // too short (2)
		"abcdefghijklmnopqrstuv", // too long (22)
		"Alice",                  // uppercase
		"-alice",                 // leading hyphen
		"_alice",                 // leading underscore
		"alice-",                 // trailing hyphen
		"alice_",                 // trailing underscore
	}
	for _, s := range invalid {
		if SlugPattern.MatchString(s) {
			t.Errorf("SlugPattern accepted invalid slug %q", s)
		}
	}
}

func TestForceClaimHyphenatedSlug(t *testing.T) {
	s, _ := setupTestServer(t)

	// Force-claim a slug with a hyphen — should succeed.
	a, reclaimed, err := s.registerAI("gpt5", "codex", "proj", nil, "foo-bar")
	if err != nil {
		t.Fatalf("registerAI with hyphenated slug: %v", err)
	}
	if reclaimed {
		t.Fatal("first registration unexpectedly reclaimed")
	}
	if a.ID != "foo-bar@proj" {
		t.Fatalf("id = %q, want foo-bar@proj", a.ID)
	}

	// Idempotent re-registration with same model/harness/project returns the
	// same agent. Force-claim always returns reclaimed=false (spawn_tag path
	// is the only way to get reclaimed=true).
	b, _, err := s.registerAI("gpt5", "codex", "proj", nil, "foo-bar")
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if b.ID != "foo-bar@proj" {
		t.Fatalf("re-registered id = %q, want foo-bar@proj", b.ID)
	}
}

func TestForceClaimInvalidSlugRejected(t *testing.T) {
	s, _ := setupTestServer(t)
	invalid := []string{"alice-", "-alice", "_bob", "ab"}
	for _, slug := range invalid {
		if _, _, err := s.registerAI("gpt5", "codex", "proj", nil, slug); err == nil {
			t.Errorf("registerAI with invalid slug %q should have failed", slug)
		}
	}
}

type roomSendResp struct {
	ID       int64    `json:"id"`
	Warnings []string `json:"warnings"`
}

func postRoomMessage(t *testing.T, baseURL, roomID, from, body string) roomSendResp {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"from": from, "body": body})
	resp, err := http.Post(baseURL+"/rooms/"+roomID+"/send", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out roomSendResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
