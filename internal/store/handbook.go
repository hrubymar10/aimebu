package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

var ErrHandbookSection = errors.New("invalid_handbook_section")
var ErrHandbookEdit = errors.New("invalid_handbook_edit")
var ErrHandbookRevision = errors.New("handbook_revision_changed")

const handbookLineLimit = 8 << 10

type HandbookLineTooLongError struct {
	Section string
	Line    int
}

func (e *HandbookLineTooLongError) Error() string {
	return fmt.Sprintf("line %d of section %s is longer than 8 KiB; split it into shorter lines", e.Line, e.Section)
}

var handbookSections = []struct{ Key, Title string }{{"current_state", "Current state"}, {"open_backlog", "Open backlog"}, {"conventions", "Conventions"}, {"gotchas", "Gotchas"}, {"timeline", "Timeline"}}
var handbookUpdatedStamp = regexp.MustCompile(`\bUpdated\s+(\d{4}-\d{2}-\d{2})\b`)

func handbookWarning(doc Handbook, now time.Time) string {
	var warnings []string
	var updated time.Time
	if match := handbookUpdatedStamp.FindStringSubmatch(doc.Sections["current_state"]); match != nil {
		updated, _ = time.Parse("2006-01-02", match[1])
	}
	if updated.IsZero() {
		updated, _ = time.Parse(time.RFC3339Nano, doc.UpdatedAt)
	}
	if !updated.IsZero() && now.Sub(updated) > 48*time.Hour {
		warnings = append(warnings, fmt.Sprintf("Current state was last updated %d h ago (over 48 h)", int(now.Sub(updated).Hours())))
	}
	backlog := doc.Sections["open_backlog"]
	lines := 0
	if backlog != "" {
		lines = strings.Count(strings.TrimSuffix(backlog, "\n"), "\n") + 1
	}
	if lines > 60 {
		warnings = append(warnings, fmt.Sprintf("Open backlog has %d lines (over 60): delete finished items", lines))
	}
	return strings.Join(warnings, "; ")
}

// Handbook is the current project document or an immutable past snapshot.
type Handbook struct {
	Rev        int64             `json:"rev"`
	Sections   map[string]string `json:"sections"`
	Markdown   string            `json:"markdown"`
	UpdatedAt  string            `json:"updated_at"`
	UpdatedBy  string            `json:"updated_by"`
	LineCount  int               `json:"line_count"`
	OverBudget bool              `json:"over_budget"`
	Nudge      string            `json:"nudge,omitempty"`
}

type HandbookRevision struct {
	Rev       int64  `json:"rev"`
	Section   string `json:"section"`
	Operation string `json:"operation"`
	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
	Diff      string `json:"diff"`
}

type HandbookHistory struct {
	Revisions     []HandbookRevision `json:"revisions"`
	NextBeforeRev *int64             `json:"next_before_rev"`
}

func emptyHandbookSections() map[string]string {
	m := make(map[string]string, len(handbookSections))
	for _, s := range handbookSections {
		m[s.Key] = ""
	}
	return m
}

func makeHandbook(rev int64, sections map[string]string, at, by string) Handbook {
	normalized := emptyHandbookSections()
	for k := range normalized {
		normalized[k] = sections[k]
	}
	var b strings.Builder
	for _, section := range handbookSections {
		fmt.Fprintf(&b, "## %s\n", section.Title)
		if content := strings.TrimSpace(normalized[section.Key]); content != "" {
			b.WriteString(content)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	md := strings.TrimSpace(b.String())
	count := strings.Count(md, "\n") + 1
	doc := Handbook{Rev: rev, Sections: normalized, Markdown: md, UpdatedAt: at, UpdatedBy: by, LineCount: count, OverBudget: count > 1000}
	if doc.OverBudget {
		doc.Nudge = "Handbook exceeds about 1000 lines; compress the oldest Timeline entries while preserving the current state and open backlog."
	}
	return doc
}

func handbookSectionOK(section string) bool {
	for _, s := range handbookSections {
		if section == s.Key {
			return true
		}
	}
	return false
}

func scanHandbook(row *sql.Row) (Handbook, error) {
	var rev int64
	var raw, at, by string
	if err := row.Scan(&rev, &raw, &at, &by); err != nil {
		return Handbook{}, err
	}
	sections := emptyHandbookSections()
	if err := json.Unmarshal([]byte(raw), &sections); err != nil {
		return Handbook{}, err
	}
	return makeHandbook(rev, sections, at, by), nil
}

func (tx *Tx) handbookForDelivery(projectID string) (Handbook, error) {
	doc, err := scanHandbook(tx.tx.QueryRowContext(tx.ctx, `SELECT rev,sections_json,updated_at,updated_by FROM project_handbook WHERE project_id=?`, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return makeHandbook(0, emptyHandbookSections(), "", ""), nil
	}
	return doc, err
}

func (s *Store) GetHandbook(ctx context.Context, projectID string) (Handbook, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return Handbook{}, err
	}
	doc, err := scanHandbook(s.db.QueryRowContext(ctx, `SELECT rev,sections_json,updated_at,updated_by FROM project_handbook WHERE project_id=?`, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project WHERE id=?)`, projectID).Scan(&exists); err != nil {
			return Handbook{}, err
		}
		if !exists {
			return Handbook{}, sql.ErrNoRows
		}
		return makeHandbook(0, emptyHandbookSections(), "", ""), nil
	}
	return doc, err
}

func (s *Store) HandbookSnapshot(ctx context.Context, projectID string, rev int64) (Handbook, error) {
	if rev == 0 {
		return s.GetHandbook(ctx, projectID)
	}
	if err := s.checkTxReentry(ctx); err != nil {
		return Handbook{}, err
	}
	return scanHandbook(s.db.QueryRowContext(ctx, `SELECT rev,sections_json,updated_at,updated_by FROM project_handbook_revision WHERE project_id=? AND rev=?`, projectID, rev))
}

func (s *Store) HandbookRevisions(ctx context.Context, projectID string, limit int, before int64) (HandbookHistory, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return HandbookHistory{}, err
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rev,section,operation,updated_at,updated_by,diff FROM project_handbook_revision WHERE project_id=? AND rev<? ORDER BY rev DESC LIMIT ?`, projectID, before, limit+1)
	if err != nil {
		return HandbookHistory{}, err
	}
	defer rows.Close()
	out := HandbookHistory{Revisions: []HandbookRevision{}}
	for rows.Next() {
		var item HandbookRevision
		if err := rows.Scan(&item.Rev, &item.Section, &item.Operation, &item.UpdatedAt, &item.UpdatedBy, &item.Diff); err != nil {
			return HandbookHistory{}, err
		}
		out.Revisions = append(out.Revisions, item)
	}
	if err := rows.Err(); err != nil {
		return HandbookHistory{}, err
	}
	if len(out.Revisions) > limit {
		out.Revisions = out.Revisions[:limit]
		next := out.Revisions[len(out.Revisions)-1].Rev
		out.NextBeforeRev = &next
	}
	return out, nil
}

// HandbookAgentAccess returns full for the lead and limited for a flow-B dispatcher.
func (s *Store) HandbookAgentAccess(ctx context.Context, projectID, agentID string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var ownProject, role, flow string
	var lead bool
	err := s.db.QueryRowContext(ctx, `SELECT a.project_id,r.role_key,p.team_flow_key,r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id JOIN project p ON p.id=a.project_id WHERE a.id=? AND a.revoked_at IS NULL`, agentID).Scan(&ownProject, &role, &flow, &lead)
	if err != nil {
		return "", err
	}
	if ownProject != projectID {
		return "", ErrForbidden
	}
	if lead {
		return "full", nil
	}
	if flow == "leader-dispatcher" && role == "dispatcher" {
		return "limited", nil
	}
	return "", ErrForbidden
}

func (s *Store) UpdateHandbook(ctx context.Context, projectID, agentID, section string, content, patch *string, rev int64) (Handbook, error) {
	if !handbookSectionOK(section) {
		return Handbook{}, ErrHandbookSection
	}
	if (content == nil) == (patch == nil) {
		return Handbook{}, ErrHandbookEdit
	}
	if rev < 0 {
		return Handbook{}, ErrHandbookEdit
	}
	var out Handbook
	err := s.Tx(ctx, func(tx *Tx) error {
		var ownProject string
		var lead bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=? AND a.revoked_at IS NULL`, agentID).Scan(&ownProject, &lead); err != nil {
			return err
		}
		if ownProject != projectID || !lead {
			return ErrForbidden
		}
		if err := tx.refuseArchived(projectID); err != nil {
			return err
		}
		sections := emptyHandbookSections()
		var oldRev int64
		var raw string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT rev,sections_json FROM project_handbook WHERE project_id=?`, projectID).Scan(&oldRev, &raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := json.Unmarshal([]byte(raw), &sections); err != nil {
				return err
			}
		}
		if rev != oldRev {
			return ErrHandbookRevision
		}
		old := sections[section]
		operation := "replace"
		if content != nil {
			sections[section] = *content
		} else {
			operation = "append"
			sections[section] = old + *patch
		}
		for i, line := range strings.Split(sections[section], "\n") {
			if len(line) > handbookLineLimit {
				return &HandbookLineTooLongError{Section: section, Line: i + 1}
			}
		}
		encoded, err := json.Marshal(sections)
		if err != nil {
			return err
		}
		now := timestamp()
		next := oldRev + 1
		if _, err := tx.Exec(`INSERT INTO project_handbook(project_id,rev,sections_json,updated_at,updated_by) VALUES (?,?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET rev=excluded.rev,sections_json=excluded.sections_json,updated_at=excluded.updated_at,updated_by=excluded.updated_by`, projectID, next, string(encoded), now, agentID); err != nil {
			return err
		}
		diff := "--- previous\n+++ current\n"
		if old != "" {
			for _, line := range strings.Split(strings.TrimSuffix(old, "\n"), "\n") {
				diff += "-" + line + "\n"
			}
		}
		if sections[section] != "" {
			for _, line := range strings.Split(strings.TrimSuffix(sections[section], "\n"), "\n") {
				diff += "+" + line + "\n"
			}
		}
		if _, err := tx.Exec(`INSERT INTO project_handbook_revision(project_id,rev,sections_json,section,operation,diff,updated_at,updated_by) VALUES (?,?,?,?,?,?,?,?)`, projectID, next, string(encoded), section, operation, diff, now, agentID); err != nil {
			return err
		}
		out = makeHandbook(next, sections, now, agentID)
		return nil
	})
	return out, err
}
