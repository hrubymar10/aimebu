package server

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/types"
)

func TestRolesCatalogLoad(t *testing.T) {
	if len(roleCatalog) != 6 {
		t.Fatalf("expected 6 catalog roles, got %d", len(roleCatalog))
	}
	for _, key := range []string{"sec-reviewer", "test-reviewer", "ux-reviewer"} {
		entry, ok := roleCatalogEntryFor(key)
		if !ok {
			t.Fatalf("missing catalog role %q", key)
		}
		if entry.Extends != "reviewer" {
			t.Fatalf("%s extends %q, want reviewer", key, entry.Extends)
		}
	}
}

func TestDefaultRolesIncludeThreeWayIndependentPlanning(t *testing.T) {
	defaults := defaultRoleBodies()
	for _, key := range []string{"leader", "worker", "reviewer"} {
		body := defaults[key]
		for _, want := range []string{
			"independent initial plan",
			"Do not read the other roles' plans before posting yours",
			"If you arrive late and another plan is already on the bus",
			"Wait for the initial plans of all others in room to be posted before discussion starts",
			"Always wait for all other agents to finish their current response",
			"Do not start a new round while another agent is still mid-message",
			"Do not casually defer scope",
			"Set needs_attention=true only when",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s default body missing %q:\n%s", key, want, body)
			}
		}
	}

	if !strings.Contains(defaults["worker"], "Do not start implementation until") {
		t.Fatalf("worker default lost implementation handoff gate:\n%s", defaults["worker"])
	}
	if !strings.Contains(defaults["worker"], "bus_read checkpoints at natural breakpoints") {
		t.Fatalf("worker default lost implementation checkpoint-read guidance:\n%s", defaults["worker"])
	}
	if !strings.Contains(defaults["reviewer"], "Review only after an implementer asks for code review") {
		t.Fatalf("reviewer default lost review-phase gate:\n%s", defaults["reviewer"])
	}
	if !strings.Contains(defaults["reviewer"], "bus_read checkpoints at natural breaks") {
		t.Fatalf("reviewer default lost long-review checkpoint-read guidance:\n%s", defaults["reviewer"])
	}
	if !strings.Contains(defaults["leader"], "each of the three initial plans") {
		t.Fatalf("leader default lost per-plan divergence audit:\n%s", defaults["leader"])
	}
	if !strings.Contains(defaults["reviewer"], "Code review is performed by review roles and coordination roles") {
		t.Fatalf("reviewer default lost CR boundary statement:\n%s", defaults["reviewer"])
	}
	if !strings.Contains(defaults["reviewer"], "do not read other reviews before posting your own") {
		t.Fatalf("reviewer default lost role-neutral CR independence:\n%s", defaults["reviewer"])
	}
}

func TestSpecialistReviewersInheritDefaultPlanningProtocol(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(defaultRoleBodies())
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"sec-reviewer", "test-reviewer", "ux-reviewer"} {
		body, ok := s.getRole(key)
		if !ok {
			t.Fatalf("expected %s role", key)
		}
		if !strings.Contains(body, "independent initial plan") {
			t.Fatalf("%s should inherit reviewer planning protocol, got:\n%s", key, body)
		}
		if !strings.Contains(body, "Always wait for all other agents to finish their current response") {
			t.Fatalf("%s should inherit reviewer sync rule, got:\n%s", key, body)
		}
		if !strings.Contains(body, "Do not casually defer scope") {
			t.Fatalf("%s should inherit reviewer scope discipline, got:\n%s", key, body)
		}
		if !strings.Contains(body, "Set needs_attention=true only when") {
			t.Fatalf("%s should inherit reviewer attention discipline, got:\n%s", key, body)
		}
	}
}

func TestRolesSetAndGet(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{
		"leader":   "leader default",
		"worker":   "worker default",
		"reviewer": "reviewer default",
	})
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Default body returned before any override
	body, ok := s.getRole("leader")
	if !ok || body != "leader default" {
		t.Fatalf("expected default body, got %q ok=%v", body, ok)
	}

	// Set an override
	if err := s.setRoleOverride("leader", "custom leader"); err != nil {
		t.Fatal(err)
	}
	body, ok = s.getRole("leader")
	if !ok || body != "custom leader" {
		t.Fatalf("expected override body, got %q", body)
	}

	// Custom role
	if err := s.setRoleOverride("my-role", "custom body"); err != nil {
		t.Fatal(err)
	}
	body, ok = s.getRole("my-role")
	if !ok || body != "custom body" {
		t.Fatalf("expected custom role body, got %q", body)
	}
}

func TestRolesValidation(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "d", "worker": "d", "reviewer": "d"})
	s, _ := newStore(dir)

	// Bad key
	if err := s.setRoleOverride("BAD KEY", "body"); err == nil {
		t.Fatal("expected error for bad key")
	}

	// Body too large
	big := make([]byte, 17*1024)
	if err := s.setRoleOverride("leader", string(big)); err == nil {
		t.Fatal("expected error for body too large")
	}
}

func TestRoleAssign(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "lead", "worker": "work", "reviewer": "rev"})
	s, _ := newStore(dir)

	// Register an agent
	agent, _, _ := s.registerAI("gpt5", "codex", "test", nil, "")
	_, _ = s.joinRoom("testroom", agent.ID)

	// Assign role
	if err := s.assignRole("testroom", agent.ID, "worker"); err != nil {
		t.Fatalf("assignRole: %v", err)
	}

	r := s.getRoom("testroom")
	if r.Roles[agent.ID] != "worker" {
		t.Fatalf("expected worker role, got %q", r.Roles[agent.ID])
	}

	// Unassign
	if err := s.assignRole("testroom", agent.ID, ""); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	r = s.getRoom("testroom")
	if r.Roles[agent.ID] != "" {
		t.Fatalf("expected empty role after unassign")
	}
}

func TestRoleDeleteConflict(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "lead", "worker": "work", "reviewer": "rev"})
	s, _ := newStore(dir)

	agent, _, _ := s.registerAI("gpt5", "codex", "test", nil, "")
	_, _ = s.joinRoom("testroom", agent.ID)
	_ = s.setCustomRole("my-role", "My Role", "", "", "custom")
	_ = s.assignRole("testroom", agent.ID, "my-role")

	// Delete without force should fail for assigned custom roles.
	if err := s.deleteRoleOverride("my-role", false); err == nil {
		t.Fatal("expected conflict error")
	}

	// Delete with force should succeed
	if err := s.deleteRoleOverride("my-role", true); err != nil {
		t.Fatalf("force delete: %v", err)
	}
}

func TestOldRoomsJSONLoads(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "d", "worker": "d", "reviewer": "d"})
	// Write schema.json first so the store doesn't wipe the data dir
	os.WriteFile(dir+"/schema.json", []byte(`{"version":2}`), 0644)
	// Write a recently-seen agent so pruneOnStartup doesn't evict the room
	now := time.Now().UTC().Format(time.RFC3339)
	agentData := `[{"id":"test-agent","name":"test-agent","kind":"human","registered_at":"` + now + `","last_seen":"` + now + `"}]`
	os.WriteFile(dir+"/agents.json", []byte(agentData), 0644)
	// Write a rooms.json without the roles field (old format); room has a member so it survives pruneOnStartup
	oldData := `[{"id":"general","members":["test-agent"],"created_at":"2025-01-01T00:00:00Z","created_by":"test"}]`
	os.WriteFile(dir+"/rooms.json", []byte(oldData), 0644)

	s, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore with old rooms.json: %v", err)
	}
	r := s.getRoom("general")
	if r == nil {
		t.Fatal("room 'general' should have loaded from old rooms.json")
	}
	if len(r.Roles) != 0 {
		t.Fatalf("expected empty Roles for old room, got %v", r.Roles)
	}
}

func TestAssignRoleSystemMessage(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "lead the team", "worker": "do the work", "reviewer": "review it"})
	s, _ := newStore(dir)

	agent, _, _ := s.registerAI("gpt5", "codex", "test", nil, "")
	_, _ = s.joinRoom("testroom", agent.ID)

	// Subscribe to room messages
	sub := s.subscribeRoom("testroom")
	defer s.unsubscribeRoom("testroom", sub)

	_ = s.assignRole("testroom", agent.ID, "worker")

	// Drain messages until we find the role assignment system message or timeout
	var assignMsg types.Message
	deadline := time.After(100 * time.Millisecond)
	draining := true
	for draining {
		select {
		case msg := <-sub:
			if strings.Contains(msg.Body, "was assigned as") {
				assignMsg = msg
			}
		case <-deadline:
			draining = false
		}
	}
	if !strings.HasPrefix(assignMsg.Body, agent.ID+" was assigned as worker") {
		t.Fatalf("expected role assignment system message, got: %q", assignMsg.Body)
	}
	if strings.Contains(assignMsg.Body, "do the work") {
		t.Fatalf("expected concise system message without role body, got: %q", assignMsg.Body)
	}
	if len(assignMsg.Targets) != 1 || assignMsg.Targets[0] != agent.Name {
		t.Fatalf("expected assignment message to target %q, got %v", agent.Name, assignMsg.Targets)
	}
	assignedView := annotate([]types.Message{assignMsg}, agent.Name, nil)
	if len(assignedView) != 1 || !assignedView[0].AddressedToMe || !assignedView[0].ShouldRespond {
		t.Fatalf("assigned agent should be woken by role assignment: %+v", assignedView)
	}
}

func TestPropagateRoleUpdate(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "old body", "worker": "w", "reviewer": "r"})
	s, _ := newStore(dir)

	agent, _, _ := s.registerAI("gpt5", "codex", "test", nil, "")
	_, _ = s.joinRoom("testroom", agent.ID)
	_ = s.assignRole("testroom", agent.ID, "leader")

	sub := s.subscribeRoom("testroom")
	defer s.unsubscribeRoom("testroom", sub)

	// Update the role body — should propagate to testroom
	_ = s.setRoleOverride("leader", "new body")

	var updateMsg string
	deadline := time.After(100 * time.Millisecond)
	draining := true
	for draining {
		select {
		case msg := <-sub:
			if strings.Contains(msg.Body, "updated") {
				updateMsg = msg.Body
			}
		case <-deadline:
			draining = false
		}
	}
	if !strings.HasPrefix(updateMsg, "role leader updated") {
		t.Fatalf("expected role update system message, got: %q", updateMsg)
	}
	if !strings.Contains(updateMsg, "new body") {
		t.Fatalf("expected new body in update message, got: %q", updateMsg)
	}
}

func TestAssignRoleErrRoomNotFound(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "l", "worker": "w", "reviewer": "r"})
	s, _ := newStore(dir)

	err := s.assignRole("nonexistent", "someagent", "worker")
	if !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("expected ErrRoomNotFound, got: %v", err)
	}
}

func TestGetRoleLabel(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "l", "worker": "w", "reviewer": "r"})
	s, _ := newStore(dir)

	// Catalog role
	if got := s.getRoleLabel("leader"); got != "leader" {
		t.Fatalf("expected 'leader', got %q", got)
	}

	// Custom role with label
	_ = s.setCustomRole("my-role", "My Role", "desc", "★", "body")
	if got := s.getRoleLabel("my-role"); got != "my-role" {
		t.Fatalf("expected 'my-role', got %q", got)
	}
	if got := s.getRoleIcon("my-role"); got != "★" {
		t.Fatalf("expected custom role emoji, got %q", got)
	}

	// Unknown role falls back to key
	if got := s.getRoleLabel("unknown-key"); got != "unknown-key" {
		t.Fatalf("expected key as fallback, got %q", got)
	}
}

func TestAssignRoleSingletonConflict(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "lead", "worker": "work", "reviewer": "review"})
	s, _ := newStore(dir)

	first, _, _ := s.registerAI("gpt5", "codex", "test", nil, "alpha")
	second, _, _ := s.registerAI("gpt5", "codex", "test", nil, "bravo")
	_, _ = s.joinRoom("room", first.ID)
	_, _ = s.joinRoom("room", second.ID)

	if err := s.assignRole("room", first.ID, "leader"); err != nil {
		t.Fatalf("assign first leader: %v", err)
	}
	if err := s.assignRole("room", second.ID, "leader"); !errors.Is(err, ErrRoleAssignmentConflict) {
		t.Fatalf("assign second leader error = %v, want ErrRoleAssignmentConflict", err)
	}
}

func TestDeregisterAgentClearsRoleBindings(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "lead", "worker": "work", "reviewer": "review"})
	s, _ := newStore(dir)

	first, _, _ := s.registerAI("gpt5", "codex", "test", nil, "alpha")
	second, _, _ := s.registerAI("gpt5", "codex", "test", nil, "bravo")
	_, _ = s.joinRoom("room", first.ID)
	_, _ = s.joinRoom("room", second.ID)

	if err := s.assignRole("room", first.ID, "leader"); err != nil {
		t.Fatalf("assign first leader: %v", err)
	}
	room := s.getRoom("room")
	if room.Roles[first.ID] != "leader" {
		t.Fatalf("expected first agent to hold leader role, got %q", room.Roles[first.ID])
	}

	if ok := s.deregisterAgent(first.ID); !ok {
		t.Fatalf("deregister first agent")
	}
	room = s.getRoom("room")
	if _, ok := room.Roles[first.ID]; ok {
		t.Fatalf("expected first agent role binding to be cleared, got %v", room.Roles)
	}

	if err := s.assignRole("room", second.ID, "leader"); err != nil {
		t.Fatalf("assign second leader after deregistration: %v", err)
	}

	var foundVacate bool
	for _, msg := range s.messagesSince("room", 0) {
		if msg.Body == "role \"leader\" vacated: "+first.ID+" deregistered" {
			foundVacate = true
			break
		}
	}
	if !foundVacate {
		t.Fatalf("expected role vacate system message")
	}
}

func TestResolvedRoleBodyExtendsBase(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "lead", "worker": "work", "reviewer": "base review"})
	s, _ := newStore(dir)

	err := s.replaceRoles([]replaceRolesEntry{
		{
			key:         "deep-sec-reviewer",
			body:        "focus on auth",
			emoji:       "🛡️",
			cardinality: "multi",
			extends:     "reviewer",
			isCustom:    true,
		},
		{
			key:         "sec-plus",
			body:        "focus on secrets",
			cardinality: "multi",
			extends:     "deep-sec-reviewer",
			isCustom:    true,
		},
	}, false)
	if err != nil {
		t.Fatalf("replaceRoles: %v", err)
	}
	body, ok := s.getRole("sec-plus")
	if !ok {
		t.Fatal("expected sec-plus role")
	}
	if !strings.Contains(body, "base review") || !strings.Contains(body, "focus on auth") || !strings.Contains(body, "focus on secrets") {
		t.Fatalf("resolved body = %q, want full extension chain", body)
	}
}

func TestBuiltInSpecialistReviewerResolvesReviewerBase(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{
		"leader":        "lead",
		"worker":        "work",
		"reviewer":      "base review",
		"sec-reviewer":  "security focus",
		"test-reviewer": "test focus",
		"ux-reviewer":   "ux focus",
	})
	s, _ := newStore(dir)

	body, ok := s.getRole("sec-reviewer")
	if !ok {
		t.Fatal("expected sec-reviewer role")
	}
	if !strings.Contains(body, "base review") || !strings.Contains(body, "security focus") {
		t.Fatalf("resolved body = %q, want reviewer base plus specialist focus", body)
	}
}

func TestReplaceRolesRejectsExtensionCycle(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "lead", "worker": "work", "reviewer": "base review"})
	s, _ := newStore(dir)

	err := s.replaceRoles([]replaceRolesEntry{
		{key: "a-reviewer", body: "a", cardinality: "multi", extends: "b-reviewer", isCustom: true},
		{key: "b-reviewer", body: "b", cardinality: "multi", extends: "a-reviewer", isCustom: true},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "extends cycle") {
		t.Fatalf("replaceRoles cycle error = %v, want extends cycle", err)
	}
}

func TestRoleNameCollisionValidation(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "lead", "worker": "work", "reviewer": "review"})
	s, _ := newStore(dir)

	if _, _, err := s.registerAI("gpt5", "codex", "test", nil, "leader"); err == nil || !strings.Contains(err.Error(), "collides with a role key") {
		t.Fatalf("register leader collision error = %v", err)
	}
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.setRoleOverride(agent.Name, "body"); err == nil || !strings.Contains(err.Error(), "collides with an active AI agent name") {
		t.Fatalf("custom role collision error = %v", err)
	}
}

func TestListAgentsWarnsLegacyRoleNameCollision(t *testing.T) {
	dir := t.TempDir()
	SetRoleDefaults(map[string]string{"leader": "lead", "worker": "work", "reviewer": "review"})
	s, _ := newStore(dir)

	s.mu.Lock()
	s.agents["leader@aimebu"] = &types.Agent{ID: "leader@aimebu", Name: "leader", Kind: "ai"}
	s.mu.Unlock()

	agents := s.listAgents()
	if len(agents) != 1 || len(agents[0].Warnings) != 1 {
		t.Fatalf("agents = %#v, want one collision warning", agents)
	}
	if !strings.Contains(agents[0].Warnings[0], "collides with a role key") {
		t.Fatalf("warning = %q, want role-key collision", agents[0].Warnings[0])
	}
}
