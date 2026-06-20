package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

const (
	maxProposedAnswers              = 4
	maxOpenQuestions                = 10
	maxOpenQuestionOptions          = 8
	maxOpenQuestionTextRunes        = 500
	maxOpenQuestionDescriptionRunes = 1000
	maxVisualPlanBlocks             = 80
	maxVisualPlanBlockTitleRunes    = 160
	maxVisualPlanBlockDataBytes     = 64000
	maxAppendixPages                = 10
	maxAppendixPageBodyBytes        = 32000
	maxAppendixPageTitleRunes       = 160
	maxAppendixTotalBodyBytes       = 128000
)

func cleanProposedAnswers(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(in), maxProposedAnswers))
	for _, answer := range in {
		answer = strings.TrimSpace(answer)
		if answer == "" {
			continue
		}
		out = append(out, answer)
		if len(out) == maxProposedAnswers {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanOpenQuestions(in []types.OpenQuestion) []types.OpenQuestion {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.OpenQuestion, 0, min(len(in), maxOpenQuestions))
	for _, q := range in {
		question := truncateRunes(strings.TrimSpace(q.Question), maxOpenQuestionTextRunes)
		description := truncateRunes(strings.TrimSpace(q.Description), maxOpenQuestionDescriptionRunes)
		if question == "" {
			continue
		}
		options := make([]string, 0, min(len(q.Options), maxOpenQuestionOptions))
		for _, opt := range q.Options {
			opt = truncateRunes(strings.TrimSpace(opt), maxOpenQuestionTextRunes)
			if opt == "" {
				continue
			}
			options = append(options, opt)
			if len(options) == maxOpenQuestionOptions {
				break
			}
		}
		if len(options) < 2 {
			continue
		}
		cleaned := types.OpenQuestion{Question: question, Options: options}
		if description != "" {
			cleaned.Description = description
		}
		out = append(out, cleaned)
		if len(out) == maxOpenQuestions {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeVisualPlanBlocks(in []types.PlanBlock) ([]types.PlanBlock, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxVisualPlanBlocks {
		return nil, fmt.Errorf("too many visual_plan blocks (max %d)", maxVisualPlanBlocks)
	}
	out := make([]types.PlanBlock, len(in))
	for i, block := range in {
		block.Type = strings.TrimSpace(block.Type)
		block.Title = strings.TrimSpace(block.Title)
		if block.ID == "" {
			block.ID = randomID()
		}
		if block.Type == "" {
			return nil, fmt.Errorf("visual_plan block type is required")
		}
		if got := utf8.RuneCountInString(block.Title); got > maxVisualPlanBlockTitleRunes {
			return nil, fmt.Errorf("visual_plan block title exceeds %d runes (%d)", maxVisualPlanBlockTitleRunes, got)
		}
		if len(block.Data) == 0 {
			block.Data = json.RawMessage(`{}`)
		}
		if len(block.Data) > maxVisualPlanBlockDataBytes {
			return nil, fmt.Errorf("visual_plan block data exceeds %d bytes (%d)", maxVisualPlanBlockDataBytes, len(block.Data))
		}
		if !json.Valid(block.Data) {
			return nil, fmt.Errorf("visual_plan block %q data must be valid JSON", block.Type)
		}
		block.Order = i
		out[i] = block
	}
	return out, nil
}

func normalizeAppendixPages(in []types.AppendixPage) ([]types.AppendixPage, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxAppendixPages {
		return nil, fmt.Errorf("too many appendix_pages (max %d)", maxAppendixPages)
	}
	out := make([]types.AppendixPage, 0, len(in))
	totalBodyBytes := 0
	for _, page := range in {
		page.Title = strings.TrimSpace(page.Title)
		page.Body = strings.TrimSpace(page.Body)
		if page.Title == "" && page.Body == "" {
			continue
		}
		if page.Body == "" {
			return nil, fmt.Errorf("appendix_pages body is required")
		}
		if got := utf8.RuneCountInString(page.Title); got > maxAppendixPageTitleRunes {
			return nil, fmt.Errorf("appendix_pages title exceeds %d runes (%d)", maxAppendixPageTitleRunes, got)
		}
		if got := len(page.Body); got > maxAppendixPageBodyBytes {
			return nil, fmt.Errorf("appendix_pages body exceeds %d bytes (%d)", maxAppendixPageBodyBytes, got)
		}
		totalBodyBytes += len(page.Body)
		if totalBodyBytes > maxAppendixTotalBodyBytes {
			return nil, fmt.Errorf("appendix_pages total body exceeds %d bytes (%d)", maxAppendixTotalBodyBytes, totalBodyBytes)
		}
		out = append(out, page)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func appendTargetDedup(targets []string, target string) []string {
	if target == "" {
		return targets
	}
	for _, existing := range targets {
		if strings.EqualFold(existing, target) {
			return targets
		}
	}
	return append(targets, target)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func memberInRoomLocked(room *types.Room, agentID string) bool {
	for _, memberID := range room.Members {
		if memberID == agentID {
			return true
		}
	}
	return false
}

func livenessSubject(roleKey, agentID string) string {
	if strings.TrimSpace(roleKey) == "" {
		return agentID
	}
	return fmt.Sprintf("%s %s", roleKey, agentID)
}

func formatDurationForHumans(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// isDM reports whether a room ID is a DM room.
func isDM(roomID string) bool {
	return strings.HasPrefix(roomID, "dm:")
}

// randomUUID returns a random 32-hex-char UUID-like string.
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validUUID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}
