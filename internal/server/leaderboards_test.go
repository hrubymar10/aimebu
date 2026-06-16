package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/types"
)

func intPtr(v int) *int { return &v }

func testLeaderboardCards(leaderID, workerID string, leaderScore, workerScore int) []types.LeaderboardRatingSubmission {
	rating := func(score int) map[string]types.LeaderboardRatingValue {
		return map[string]types.LeaderboardRatingValue{
			types.LeaderboardCategoryTaskOutcome:          {Score: intPtr(score)},
			types.LeaderboardCategoryRoleExecution:        {Score: intPtr(score)},
			types.LeaderboardCategoryCollaborationProcess: {Score: intPtr(3)},
			types.LeaderboardCategoryJudgmentScope:        {Score: intPtr(score)},
			types.LeaderboardCategoryContextUnderstanding: {Score: nil},
		}
	}
	return []types.LeaderboardRatingSubmission{
		{Subject: leaderID, Ratings: rating(leaderScore)},
		{Subject: workerID, Ratings: rating(workerScore)},
	}
}

func setupLeaderboardStore(t *testing.T) (*store, types.Agent, types.Agent) {
	t.Helper()
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leader, _, err := s.registerAI("gpt5", "codex", "test", nil, "leadone")
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := s.registerAI("opus4.7", "claude-code", "test", nil, "workone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("rated", leader.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("rated", worker.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.assignRole("rated", leader.ID, "leader"); err != nil {
		t.Fatal(err)
	}
	if err := s.assignRole("rated", worker.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	return s, *leader, *worker
}

func TestLeaderboardStartPostsInWorkingRoomOnly(t *testing.T) {
	s, leader, _ := setupLeaderboardStore(t)

	participants, err := s.startLeaderboardVoting(leader.ID, "rated")
	if err != nil {
		t.Fatal(err)
	}
	if len(participants) != 2 {
		t.Fatalf("participants = %d, want 2", len(participants))
	}
	if participants[0].RoleKey == "" || participants[1].RoleKey == "" {
		t.Fatalf("participants should include live role keys: %+v", participants)
	}

	s.mu.RLock()
	_, hasLeaderboardRoom := s.rooms["leaderboard"]
	ratedMessages := append([]types.Message{}, s.messages["rated"]...)
	s.mu.RUnlock()
	if hasLeaderboardRoom {
		t.Fatal("start created dedicated leaderboard room")
	}
	if len(ratedMessages) == 0 {
		t.Fatal("start did not post system message in rated room")
	}
	got := ratedMessages[len(ratedMessages)-1]
	if got.From != "_system" || !strings.Contains(got.Body, "Leader started a voting session") {
		t.Fatalf("latest rated message = %+v, want voting-session system message", got)
	}
}

func TestLeaderboardRejectsNonLeaderStart(t *testing.T) {
	s, _, worker := setupLeaderboardStore(t)

	if _, err := s.startLeaderboardVoting(worker.ID, "rated"); err == nil {
		t.Fatal("non-leader started leaderboard voting")
	}
}

func TestLeaderboardFlatCardsPeerOnlyAggregate(t *testing.T) {
	s, leader, worker := setupLeaderboardStore(t)

	if _, err := s.submitLeaderboardCards(leader.ID, testLeaderboardCards(leader.ID, worker.ID, 5, 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.submitLeaderboardCards(worker.ID, testLeaderboardCards(leader.ID, worker.ID, 2, 5)); err != nil {
		t.Fatal(err)
	}

	view, err := s.leaderboardView("overall", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Aggregates) != 2 {
		t.Fatalf("aggregates = %d, want 2", len(view.Aggregates))
	}
	var workerAgg types.LeaderboardAggregate
	for _, agg := range view.Aggregates {
		if agg.Model == worker.Model && agg.Harness == worker.Harness {
			workerAgg = agg
		}
	}
	if workerAgg.Key == "" {
		t.Fatalf("worker aggregate not found in %+v", view.Aggregates)
	}
	if workerAgg.Cards != 1 {
		t.Fatalf("peer-only worker sample = cards:%d, want 1", workerAgg.Cards)
	}
	if workerAgg.NACounts[types.LeaderboardCategoryContextUnderstanding] == 0 {
		t.Fatalf("expected N/A counts for context understanding, got %+v", workerAgg.NACounts)
	}
	if workerAgg.Categories[types.LeaderboardCategoryContextUnderstanding] != 0 {
		t.Fatalf("N/A category should not have a mean, got %+v", workerAgg.Categories)
	}
	if workerAgg.Overall != 3.75 {
		t.Fatalf("peer-only worker overall = %.2f, want 3.75", workerAgg.Overall)
	}
	if workerAgg.PeerOnlyOverall != workerAgg.Overall {
		t.Fatalf("peer-only baseline = %.2f, want %.2f", workerAgg.PeerOnlyOverall, workerAgg.Overall)
	}
	if workerAgg.SelfDelta <= 0 {
		t.Fatalf("expected positive self delta for worker aggregate, got %.2f", workerAgg.SelfDelta)
	}
	if got := view.Summary["total_cards"]; got != 4 {
		t.Fatalf("summary total_cards = %v, want 4", got)
	}

	withSelf, err := s.leaderboardView("overall", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, agg := range withSelf.Aggregates {
		if agg.Model == worker.Model && agg.Harness == worker.Harness && agg.Overall <= workerAgg.Overall {
			t.Fatalf("including worker self-review should raise worker overall, peer=%.2f with_self=%.2f", workerAgg.Overall, agg.Overall)
		}
	}
}

func TestLeaderboardSubmitAppendsAnonymousCards(t *testing.T) {
	s, leader, worker := setupLeaderboardStore(t)

	if _, err := s.submitLeaderboardCards(leader.ID, testLeaderboardCards(leader.ID, worker.ID, 5, 4)); err != nil {
		t.Fatal(err)
	}
	more := testLeaderboardCards(leader.ID, worker.ID, 3, 3)
	if _, err := s.submitLeaderboardCards(leader.ID, more); err != nil {
		t.Fatal(err)
	}

	s.leaderboardsMu.RLock()
	cardCount := len(s.leaderboards)
	s.leaderboardsMu.RUnlock()
	if cardCount != 4 {
		t.Fatalf("stored cards = %d, want 4 after append", cardCount)
	}

	view, err := s.leaderboardView("overall", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Aggregates) != 1 {
		t.Fatalf("peer-only aggregates = %d, want 1: %+v", len(view.Aggregates), view.Aggregates)
	}
	agg := view.Aggregates[0]
	if agg.Model != worker.Model || agg.Cards != 2 || agg.Overall != 3.38 {
		t.Fatalf("append aggregate = %+v, want worker 2 cards overall 3.38", agg)
	}
}

func TestLeaderboardRejectsInvalidRatings(t *testing.T) {
	cases := []struct {
		name  string
		score *int
	}{
		{name: "too low", score: intPtr(0)},
		{name: "too high", score: intPtr(6)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, leader, worker := setupLeaderboardStore(t)
			cards := testLeaderboardCards(leader.ID, worker.ID, 5, 4)
			cards[0].Ratings[types.LeaderboardCategoryTaskOutcome] = types.LeaderboardRatingValue{Score: tc.score}
			_, err := s.submitLeaderboardCards(leader.ID, cards)
			if err == nil {
				t.Fatalf("submit with %s score succeeded", tc.name)
			}
		})
	}
}

func TestLeaderboardAcceptsNullScores(t *testing.T) {
	s, leader, worker := setupLeaderboardStore(t)
	cards := testLeaderboardCards(leader.ID, worker.ID, 5, 4)
	cards[0].Ratings[types.LeaderboardCategoryTaskOutcome] = types.LeaderboardRatingValue{Score: nil}
	if _, err := s.submitLeaderboardCards(leader.ID, cards); err != nil {
		t.Fatal(err)
	}
}

func TestLeaderboardTrendOrdersByCreatedAt(t *testing.T) {
	s, _, worker := setupLeaderboardStore(t)
	cards := []types.LeaderboardRatingCard{
		storedLeaderboardCard(worker.Model, worker.Harness, false, "2026-01-02T00:00:00Z", 4),
		storedLeaderboardCard(worker.Model, worker.Harness, false, "2026-01-01T00:00:00Z", 2),
	}
	s.leaderboardsMu.Lock()
	s.leaderboards = append(s.leaderboards, cards...)
	s.leaderboardsMu.Unlock()

	view, err := s.leaderboardView("overall", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Aggregates) != 1 {
		t.Fatalf("aggregates = %d, want 1", len(view.Aggregates))
	}
	trend := view.Aggregates[0].RecentTrend
	if len(trend) != 2 || trend[0] != 2 || trend[1] != 4 {
		t.Fatalf("trend = %+v, want [2 4]", trend)
	}
}

func storedLeaderboardCard(model, harness string, isSelfReview bool, createdAt string, score int) types.LeaderboardRatingCard {
	ratings := make(map[string]types.LeaderboardRatingValue, len(leaderboardCategories))
	for _, category := range leaderboardCategories {
		ratings[category] = types.LeaderboardRatingValue{Score: intPtr(score)}
	}
	return types.LeaderboardRatingCard{
		SubjectModel:   model,
		SubjectHarness: harness,
		IsSelfReview:   isSelfReview,
		Ratings:        ratings,
		CreatedAt:      createdAt,
	}
}

func TestLeaderboardsPersistAcrossStoreReload(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	leader, _, err := s.registerAI("gpt5", "codex", "test", nil, "leadone")
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := s.registerAI("opus4.7", "claude-code", "test", nil, "workone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.submitLeaderboardCards(leader.ID, testLeaderboardCards(leader.ID, worker.ID, 5, 4)); err != nil {
		t.Fatal(err)
	}

	reloaded, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	view, err := reloaded.leaderboardView("overall", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Aggregates) != 2 {
		t.Fatalf("reloaded aggregates = %d, want 2", len(view.Aggregates))
	}
}

func TestLeaderboardsDropOldRoundSchemaOnLoad(t *testing.T) {
	dir := t.TempDir()
	old := []byte(`{"rounds":[{"id":"old","room_id":"rated","task":"leaky","cards":[]}]} `)
	if err := os.WriteFile(dir+"/leaderboards.json", old, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.leaderboardView("overall", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Summary["total_cards"]; got != 0 {
		t.Fatalf("total_cards = %v, want 0 for old round schema", got)
	}
}

func TestLeaderboardsHTTPRoundTrip(t *testing.T) {
	s, srv := setupTestServer(t)
	leader, _, err := s.registerAI("gpt5", "codex", "test", nil, "leadone")
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := s.registerAI("opus4.7", "claude-code", "test", nil, "workone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("rated", leader.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("rated", worker.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.assignRole("rated", leader.ID, "leader"); err != nil {
		t.Fatal(err)
	}

	var startResp struct {
		Participants []types.LeaderboardParticipant `json:"participants"`
	}
	postJSON(t, srv.URL+"/leaderboard/start", map[string]any{"agent_id": leader.ID, "room": "rated"}, &startResp)
	if len(startResp.Participants) != 2 {
		t.Fatalf("participants = %d, want 2", len(startResp.Participants))
	}
	postJSON(t, srv.URL+"/leaderboard/cards", map[string]any{
		"agent_id": leader.ID,
		"cards":    testLeaderboardCards(leader.ID, worker.ID, 5, 4),
	}, nil)

	resp, err := http.Get(srv.URL + "/leaderboard?exclude_self=false")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /leaderboard status = %d", resp.StatusCode)
	}
	var view types.LeaderboardView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if len(view.Aggregates) != 2 {
		t.Fatalf("aggregates = %d, want 2", len(view.Aggregates))
	}
}

func postJSON(t *testing.T, url string, body any, out any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("POST %s status = %d", url, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}
