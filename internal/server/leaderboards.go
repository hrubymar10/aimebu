package server

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

const leaderboardHighVarianceThreshold = 1.25

var leaderboardCategories = []string{
	types.LeaderboardCategoryTaskOutcome,
	types.LeaderboardCategoryRoleExecution,
	types.LeaderboardCategoryCollaborationProcess,
	types.LeaderboardCategoryJudgmentScope,
	types.LeaderboardCategoryContextUnderstanding,
}

var leaderboardCategoryLabels = map[string]string{
	types.LeaderboardCategoryTaskOutcome:          "Task Outcome",
	types.LeaderboardCategoryRoleExecution:        "Role Execution",
	types.LeaderboardCategoryCollaborationProcess: "Collaboration & Process",
	types.LeaderboardCategoryJudgmentScope:        "Judgment & Scope",
	types.LeaderboardCategoryContextUnderstanding: "Context Understanding",
}

type leaderboardsEnvelope struct {
	Cards []types.LeaderboardRatingCard `json:"cards"`
}

func (s *store) loadLeaderboards() {
	if s.db != nil {
		var cards []types.LeaderboardRatingCard
		if err := s.loadJSONRows("leaderboards", "seq", func(_ string, data []byte) error {
			var card types.LeaderboardRatingCard
			if err := json.Unmarshal(data, &card); err != nil {
				return err
			}
			card = normalizeStoredLeaderboardCard(card)
			if card.SubjectModel == "" && card.SubjectHarness == "" {
				return nil
			}
			cards = append(cards, card)
			return nil
		}); err == nil {
			s.leaderboardsMu.Lock()
			s.leaderboards = cards
			s.leaderboardsMu.Unlock()
			return
		}
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "leaderboards.json"))
	if err != nil {
		return
	}
	var env leaderboardsEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	s.leaderboardsMu.Lock()
	s.leaderboards = make([]types.LeaderboardRatingCard, 0, len(env.Cards))
	for _, card := range env.Cards {
		card = normalizeStoredLeaderboardCard(card)
		if card.SubjectModel == "" && card.SubjectHarness == "" {
			continue
		}
		s.leaderboards = append(s.leaderboards, card)
	}
	s.leaderboardsMu.Unlock()
}

func normalizeStoredLeaderboardCard(card types.LeaderboardRatingCard) types.LeaderboardRatingCard {
	card.SubjectModel = strings.TrimSpace(card.SubjectModel)
	card.SubjectHarness = strings.TrimSpace(card.SubjectHarness)
	card.CreatedAt = strings.TrimSpace(card.CreatedAt)
	return card
}

func (s *store) persistLeaderboardsLocked() {
	if s.db != nil {
		rows := make(map[string]any, len(s.leaderboards))
		for i, card := range s.leaderboards {
			rows[fmt.Sprintf("%012d", i+1)] = card
		}
		if err := s.replaceJSONRows("leaderboards", "seq", rows); err != nil {
			log.Printf("aimebu: persist leaderboards sqlite: %v", err)
		}
		return
	}
	cards := append([]types.LeaderboardRatingCard{}, s.leaderboards...)
	sortLeaderboardCards(cards)
	if data, err := json.MarshalIndent(leaderboardsEnvelope{Cards: cards}, "", "  "); err == nil {
		atomicWrite(filepath.Join(s.dir, "leaderboards.json"), data)
	}
}

func sortLeaderboardCards(cards []types.LeaderboardRatingCard) {
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].CreatedAt == cards[j].CreatedAt {
			if cards[i].SubjectModel == cards[j].SubjectModel {
				if cards[i].SubjectHarness == cards[j].SubjectHarness {
					return !cards[i].IsSelfReview && cards[j].IsSelfReview
				}
				return cards[i].SubjectHarness < cards[j].SubjectHarness
			}
			return cards[i].SubjectModel < cards[j].SubjectModel
		}
		return cards[i].CreatedAt < cards[j].CreatedAt
	})
}

func (s *store) clearLeaderboards() {
	s.leaderboardsMu.Lock()
	s.leaderboards = []types.LeaderboardRatingCard{}
	if s.db != nil {
		if err := s.clearTable("leaderboards"); err != nil {
			log.Printf("aimebu: clear leaderboards sqlite: %v", err)
		}
		s.leaderboardsMu.Unlock()
		return
	}
	s.leaderboardsMu.Unlock()
	_ = os.Remove(filepath.Join(s.dir, "leaderboards.json"))
}

func (s *store) leaderboardsEnabled() bool {
	set := s.getSettings()
	return set.LeaderboardEnabled == nil || *set.LeaderboardEnabled
}

func validLeaderboardCategory(category string) bool {
	for _, c := range leaderboardCategories {
		if category == c {
			return true
		}
	}
	return false
}

func normalizeLeaderboardCategory(category string) (string, error) {
	category = strings.TrimSpace(category)
	if category == "" || category == "overall" {
		return "overall", nil
	}
	if !validLeaderboardCategory(category) {
		return "", fmt.Errorf("invalid category %q", category)
	}
	return category, nil
}

func (s *store) startLeaderboardVoting(agentID, roomID string) ([]types.LeaderboardParticipant, error) {
	if !s.leaderboardsEnabled() {
		return nil, fmt.Errorf("leaderboards are disabled")
	}
	participants, err := s.leaderboardParticipantsForRoom(agentID, roomID, true)
	if err != nil {
		return nil, err
	}
	body := fmt.Sprintf("Leader started a voting session for the recent task in room %s.\nRate the current AI members: %s\nSubmit one numeric card for each participant, including yourself. Self-reviews are recorded but excluded from default aggregates.", roomID, participantNames(participants))
	s.emitSystemMessageTo(roomID, body, participantTargets(participants))
	return participants, nil
}

func (s *store) leaderboardParticipantsForRoom(agentID, roomID string, requireLeader bool) ([]types.LeaderboardParticipant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	room, ok := s.rooms[roomID]
	if !ok {
		return nil, ErrRoomNotFound
	}
	if requireLeader && (room.Roles == nil || room.Roles[agentID] != "leader") {
		return nil, fmt.Errorf("agent %s is not leader in room %s", agentID, roomID)
	}
	participants := make([]types.LeaderboardParticipant, 0, len(room.Members))
	for _, memberID := range room.Members {
		agent := s.agents[memberID]
		if agent == nil || agent.Kind != "ai" {
			continue
		}
		roleKey := ""
		if room.Roles != nil {
			roleKey = room.Roles[memberID]
		}
		participants = append(participants, types.LeaderboardParticipant{
			AgentID: agent.ID,
			Slug:    agent.Name,
			Model:   agent.Model,
			Harness: agent.Harness,
			RoleKey: roleKey,
		})
	}
	if len(participants) == 0 {
		return nil, fmt.Errorf("room %s has no AI participants", roomID)
	}
	sort.Slice(participants, func(i, j int) bool {
		return participants[i].AgentID < participants[j].AgentID
	})
	return participants, nil
}

func participantNames(participants []types.LeaderboardParticipant) string {
	parts := make([]string, 0, len(participants))
	for _, p := range participants {
		label := p.AgentID
		if p.Model != "" || p.Harness != "" {
			label += " (" + p.Model + "/" + p.Harness + ")"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

func participantTargets(participants []types.LeaderboardParticipant) []string {
	targets := make([]string, 0, len(participants))
	for _, p := range participants {
		targets = append(targets, p.AgentID)
	}
	return targets
}

func (s *store) submitLeaderboardCards(agentID string, cards []types.LeaderboardRatingSubmission) ([]types.LeaderboardRatingCard, error) {
	if !s.leaderboardsEnabled() {
		return nil, fmt.Errorf("leaderboards are disabled")
	}
	if len(cards) == 0 {
		return nil, fmt.Errorf("at least one card is required")
	}

	s.mu.RLock()
	reviewer := s.agents[agentID]
	if reviewer == nil || reviewer.Kind != "ai" {
		s.mu.RUnlock()
		return nil, fmt.Errorf("agent %s is not a registered AI agent", agentID)
	}
	subjects := make(map[string]types.Agent, len(cards))
	for _, card := range cards {
		subjectID := strings.TrimSpace(card.Subject)
		if subjectID == "" {
			s.mu.RUnlock()
			return nil, fmt.Errorf("card subject is required")
		}
		agent := s.agents[subjectID]
		if agent == nil || agent.Kind != "ai" {
			s.mu.RUnlock()
			return nil, fmt.Errorf("subject %s is not a registered AI agent", subjectID)
		}
		subjects[subjectID] = *agent
	}
	s.mu.RUnlock()

	nowStr := now()
	cleaned := make([]types.LeaderboardRatingCard, 0, len(cards))
	for _, card := range cards {
		subject := subjects[strings.TrimSpace(card.Subject)]
		if err := validateLeaderboardRatings(card.Ratings); err != nil {
			return nil, fmt.Errorf("card for %s: %w", subject.ID, err)
		}
		cleaned = append(cleaned, types.LeaderboardRatingCard{
			SubjectModel:   subject.Model,
			SubjectHarness: subject.Harness,
			IsSelfReview:   agentID == subject.ID,
			Ratings:        card.Ratings,
			CreatedAt:      nowStr,
		})
	}

	s.leaderboardsMu.Lock()
	s.leaderboards = append(s.leaderboards, cleaned...)
	s.persistLeaderboardsLocked()
	s.leaderboardsMu.Unlock()
	s.broadcastLeaderboardUpdate()
	return cleaned, nil
}

func validateLeaderboardRatings(ratings map[string]types.LeaderboardRatingValue) error {
	if len(ratings) != len(leaderboardCategories) {
		return fmt.Errorf("ratings must include all %d categories", len(leaderboardCategories))
	}
	for _, category := range leaderboardCategories {
		rating, ok := ratings[category]
		if !ok {
			return fmt.Errorf("missing category %s", category)
		}
		if rating.Score == nil {
			continue
		}
		if *rating.Score < 1 || *rating.Score > 5 {
			return fmt.Errorf("%s score must be 1-5 or null", category)
		}
	}
	for category := range ratings {
		if !validLeaderboardCategory(category) {
			return fmt.Errorf("invalid category %s", category)
		}
	}
	return nil
}

func (s *store) leaderboardView(category string, excludeSelf bool) (types.LeaderboardView, error) {
	category, err := normalizeLeaderboardCategory(category)
	if err != nil {
		return types.LeaderboardView{}, err
	}
	s.leaderboardsMu.RLock()
	cards := append([]types.LeaderboardRatingCard{}, s.leaderboards...)
	s.leaderboardsMu.RUnlock()
	sortLeaderboardCards(cards)

	peerAggregates := computeLeaderboardAggregates(cards, true, false)
	selfAggregates := computeLeaderboardAggregates(cards, false, false)
	aggregates := peerAggregates
	if !excludeSelf {
		aggregates = selfAggregates
	}
	annotateLeaderboardSelfDeltas(aggregates, peerAggregates, selfAggregates)

	peerModelRollups := computeLeaderboardAggregates(cards, true, true)
	selfModelRollups := computeLeaderboardAggregates(cards, false, true)
	modelRollups := peerModelRollups
	if !excludeSelf {
		modelRollups = selfModelRollups
	}
	annotateLeaderboardSelfDeltas(modelRollups, peerModelRollups, selfModelRollups)

	selected := rankLeaderboardAggregates(aggregates, category)
	modelRollups = rankLeaderboardAggregates(modelRollups, category)

	summary := map[string]any{
		"total_cards":       len(cards),
		"total_combos":      len(selected),
		"category_labels":   leaderboardCategoryLabels,
		"self_reviews_mode": map[bool]string{true: "excluded", false: "included"}[excludeSelf],
		"latest_rating_at":  latestLeaderboardRatingAt(cards),
	}
	if len(selected) > 0 {
		summary["best_combo"] = selected[0]
	}
	return types.LeaderboardView{
		Enabled:            s.leaderboardsEnabled(),
		Category:           category,
		ExcludeSelfReviews: excludeSelf,
		Categories:         append([]string{}, leaderboardCategories...),
		Aggregates:         selected,
		ModelRollups:       modelRollups,
		Summary:            summary,
	}, nil
}

func latestLeaderboardRatingAt(cards []types.LeaderboardRatingCard) string {
	latest := ""
	for _, card := range cards {
		if card.CreatedAt > latest {
			latest = card.CreatedAt
		}
	}
	return latest
}

type aggregateAccumulator struct {
	model       string
	harness     string
	category    map[string][]float64
	naCounts    map[string]int
	cards       int
	ratings     int
	allScores   []float64
	recentTrend []float64
	lastRatedAt string
}

func computeLeaderboardAggregates(cards []types.LeaderboardRatingCard, excludeSelf, modelOnly bool) []types.LeaderboardAggregate {
	accs := map[string]*aggregateAccumulator{}
	for _, card := range cards {
		if excludeSelf && card.IsSelfReview {
			continue
		}
		key := card.SubjectModel + "|" + card.SubjectHarness
		harness := card.SubjectHarness
		if modelOnly {
			key = card.SubjectModel
			harness = ""
		}
		if key == "" {
			continue
		}
		acc := accs[key]
		if acc == nil {
			acc = &aggregateAccumulator{
				model:    card.SubjectModel,
				harness:  harness,
				category: map[string][]float64{},
				naCounts: map[string]int{},
			}
			accs[key] = acc
		}
		acc.cards++
		acc.lastRatedAt = card.CreatedAt
		var cardScores []float64
		for _, category := range leaderboardCategories {
			rating, ok := card.Ratings[category]
			if !ok || rating.Score == nil {
				acc.naCounts[category]++
				continue
			}
			score := float64(*rating.Score)
			acc.category[category] = append(acc.category[category], score)
			acc.allScores = append(acc.allScores, score)
			cardScores = append(cardScores, score)
			acc.ratings++
		}
		if len(cardScores) > 0 {
			acc.recentTrend = append(acc.recentTrend, mean(cardScores))
			if len(acc.recentTrend) > 10 {
				acc.recentTrend = acc.recentTrend[len(acc.recentTrend)-10:]
			}
		}
	}

	out := make([]types.LeaderboardAggregate, 0, len(accs))
	for key, acc := range accs {
		cats := make(map[string]float64, len(leaderboardCategories))
		counts := make(map[string]int, len(leaderboardCategories))
		var catMeans []float64
		for _, category := range leaderboardCategories {
			values := acc.category[category]
			counts[category] = len(values)
			if len(values) == 0 {
				continue
			}
			m := mean(values)
			cats[category] = m
			catMeans = append(catMeans, m)
		}
		overall := mean(catMeans)
		variance := variance(acc.allScores)
		out = append(out, types.LeaderboardAggregate{
			Key:          key,
			Model:        acc.model,
			Harness:      acc.harness,
			Overall:      round2(overall),
			Categories:   roundMap(cats),
			Counts:       counts,
			NACounts:     acc.naCounts,
			Cards:        acc.cards,
			Ratings:      acc.ratings,
			HighVariance: variance >= leaderboardHighVarianceThreshold,
			Variance:     round2(variance),
			RecentTrend:  roundFloatSlice(acc.recentTrend),
			LastRatedAt:  acc.lastRatedAt,
			SelfIncluded: !excludeSelf,
		})
	}
	return out
}

func annotateLeaderboardSelfDeltas(aggregates, peerOnly, selfIncluded []types.LeaderboardAggregate) {
	peerByKey := make(map[string]float64, len(peerOnly))
	for _, agg := range peerOnly {
		peerByKey[agg.Key] = agg.Overall
	}
	selfByKey := make(map[string]float64, len(selfIncluded))
	for _, agg := range selfIncluded {
		selfByKey[agg.Key] = agg.Overall
	}
	for i := range aggregates {
		peer, ok := peerByKey[aggregates[i].Key]
		if ok {
			aggregates[i].PeerOnlyOverall = peer
		}
		self, ok := selfByKey[aggregates[i].Key]
		if ok && aggregates[i].PeerOnlyOverall != 0 {
			aggregates[i].SelfDelta = round2(self - aggregates[i].PeerOnlyOverall)
		}
	}
}

func rankLeaderboardAggregates(in []types.LeaderboardAggregate, category string) []types.LeaderboardAggregate {
	out := append([]types.LeaderboardAggregate{}, in...)
	score := func(a types.LeaderboardAggregate) float64 {
		if category == "overall" {
			return a.Overall
		}
		return a.Categories[category]
	}
	sort.Slice(out, func(i, j int) bool {
		si, sj := score(out[i]), score(out[j])
		if si == sj {
			if out[i].Ratings == out[j].Ratings {
				return out[i].Key < out[j].Key
			}
			return out[i].Ratings > out[j].Ratings
		}
		return si > sj
	})
	return out
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func variance(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	m := mean(values)
	var sum float64
	for _, v := range values {
		d := v - m
		sum += d * d
	}
	return sum / float64(len(values))
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func roundMap(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = round2(v)
	}
	return out
}

func roundFloatSlice(in []float64) []float64 {
	out := make([]float64, 0, len(in))
	for _, v := range in {
		out = append(out, round2(v))
	}
	return out
}

func (s *store) broadcastLeaderboardUpdate() {
	s.broadcastMeta(MetaEvent{Type: "leaderboard_updated", Data: map[string]any{}})
}
