package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrScheduleInvalid = errors.New("invalid schedule")
var ErrScheduleLimit = errors.New("schedule limit")

type Schedule struct {
	ID              string `json:"id"`
	RoomID          string `json:"room_id"`
	ProjectID       string `json:"project_id"`
	CreatorKind     string `json:"creator_kind"`
	CreatorID       string `json:"creator_id"`
	TargetAgentID   string `json:"target_agent_id,omitempty"`
	TargetLead      bool   `json:"target_lead"`
	Body            string `json:"body"`
	Kind            string `json:"kind"`
	Timezone        string `json:"timezone"`
	IntervalSeconds int64  `json:"interval_seconds,omitempty"`
	LocalTime       string `json:"local_time,omitempty"`
	Weekday         int    `json:"weekday,omitempty"`
	Enabled         bool   `json:"enabled"`
	NextRunAt       string `json:"next_run_at"`
	LastOutcome     string `json:"last_outcome,omitempty"`
	LastReason      string `json:"last_reason,omitempty"`
	LastRunAt       string `json:"last_run_at,omitempty"`
	Rev             int64  `json:"rev"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// NextScheduleTime chooses the first future occurrence. For civil times it
// scans real instants: a spring gap moves forward and a fall overlap uses the
// first occurrence, exactly once on that local date.
func NextScheduleTime(s Schedule, after time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}, ErrScheduleInvalid
	}
	switch s.Kind {
	case "once":
		at, err := time.Parse(timestampLayout, s.NextRunAt)
		if err != nil || !at.After(after) {
			return time.Time{}, ErrScheduleInvalid
		}
		return at, nil
	case "interval":
		if s.IntervalSeconds < 300 {
			return time.Time{}, ErrScheduleInvalid
		}
		base, err := time.Parse(timestampLayout, s.NextRunAt)
		if err != nil {
			return time.Time{}, ErrScheduleInvalid
		}
		step := time.Duration(s.IntervalSeconds) * time.Second
		if base.After(after) {
			return base, nil
		}
		return base.Add((after.Sub(base)/step + 1) * step), nil
	case "daily", "weekly":
		parts := strings.Split(s.LocalTime, ":")
		if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
			return time.Time{}, ErrScheduleInvalid
		}
		var hour, minute int
		if _, err = fmt.Sscanf(s.LocalTime, "%02d:%02d", &hour, &minute); err != nil || hour > 23 || minute > 59 {
			return time.Time{}, ErrScheduleInvalid
		}
		if s.Kind == "weekly" && (s.Weekday < 0 || s.Weekday > 6) {
			return time.Time{}, ErrScheduleInvalid
		}
		day := after.In(loc)
		for i := 0; i < 9; i++ {
			y, m, d := day.Date()
			if s.Kind == "daily" || int(day.Weekday()) == s.Weekday {
				start := time.Date(y, m, d, 0, 0, 0, 0, loc)
				// Begin before local midnight to cover zones with midnight transitions.
				for instant := start.Add(-3 * time.Hour); instant.Before(start.Add(27 * time.Hour)); instant = instant.Add(time.Minute) {
					local := instant.In(loc)
					yy, mm, dd := local.Date()
					if yy == y && mm == m && dd == d && (local.Hour() > hour || local.Hour() == hour && local.Minute() >= minute) {
						if instant.After(after) {
							return instant.UTC(), nil
						}
						break
					}
				}
			}
			day = time.Date(y, m, d+1, 12, 0, 0, 0, loc)
		}
	}
	return time.Time{}, ErrScheduleInvalid
}

func validateSchedule(s Schedule, now time.Time) (Schedule, error) {
	s.Body = strings.TrimSpace(s.Body)
	if s.Body == "" || utf8.RuneCountInString(s.Body) > 2000 || len(s.Body) > 8000 || (s.TargetAgentID == "") == !s.TargetLead {
		return s, ErrScheduleInvalid
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil || s.Timezone == "Local" {
		return s, ErrScheduleInvalid
	}
	switch s.Kind {
	case "once":
		at, err := time.Parse(time.RFC3339, s.NextRunAt)
		if err != nil || !at.After(now) {
			return s, ErrScheduleInvalid
		}
		s.NextRunAt = at.UTC().Format(timestampLayout)
	case "interval":
		if s.IntervalSeconds < 300 || s.IntervalSeconds > 365*24*3600 {
			return s, ErrScheduleInvalid
		}
		s.NextRunAt = now.Add(time.Duration(s.IntervalSeconds) * time.Second).UTC().Format(timestampLayout)
	case "daily", "weekly":
		// Seed NextRunAt for the recurrence helper; its value is used only by interval.
		s.NextRunAt = now.UTC().Format(timestampLayout)
		at, err := NextScheduleTime(s, now)
		if err != nil {
			return s, err
		}
		s.NextRunAt = at.Format(timestampLayout)
	default:
		return s, ErrScheduleInvalid
	}
	return s, nil
}

func scanSchedule(row interface{ Scan(...any) error }) (Schedule, error) {
	var s Schedule
	var target, lastRun sql.NullString
	err := row.Scan(&s.ID, &s.RoomID, &s.ProjectID, &s.CreatorKind, &s.CreatorID, &target, &s.TargetLead, &s.Body, &s.Kind, &s.Timezone, &s.IntervalSeconds, &s.LocalTime, &s.Weekday, &s.Enabled, &s.NextRunAt, &s.LastOutcome, &s.LastReason, &lastRun, &s.Rev, &s.CreatedAt, &s.UpdatedAt)
	s.TargetAgentID = target.String
	s.LastRunAt = lastRun.String
	return s, err
}

const scheduleColumns = `id,room_id,project_id,creator_kind,creator_id,target_agent_id,target_lead,body,kind,timezone,interval_seconds,local_time,weekday,enabled,next_run_at,last_outcome,last_reason,last_run_at,rev,created_at,updated_at`

func (s *Store) ListSchedules(ctx context.Context, roomID string) ([]Schedule, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedule WHERE room_id=? ORDER BY created_at,id`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		item, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) AgentInRoom(ctx context.Context, agentID, roomID string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var joined bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`, roomID, agentID).Scan(&joined)
	return joined, err
}

func (tx *Tx) CreateSchedule(item Schedule, now time.Time) (Schedule, error) {
	var kind, lifecycle, projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT kind,lifecycle,project_id FROM room WHERE id=?`, item.RoomID).Scan(&kind, &lifecycle, &projectID); err != nil {
		return Schedule{}, err
	}
	if kind != "task" || lifecycle != "open" {
		return Schedule{}, ErrScheduleInvalid
	}
	if item.ProjectID != "" && item.ProjectID != projectID {
		return Schedule{}, ErrForbidden
	}
	item.ProjectID = projectID
	if !item.TargetLead {
		var joined bool
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`, item.RoomID, item.TargetAgentID).Scan(&joined)
		if err != nil {
			return Schedule{}, err
		}
		if !joined {
			return Schedule{}, ErrScheduleInvalid
		}
	}
	var count int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT count(*) FROM schedule WHERE room_id=? AND enabled=1`, item.RoomID).Scan(&count); err != nil {
		return Schedule{}, err
	}
	if count >= 20 {
		return Schedule{}, ErrScheduleLimit
	}
	item, err := validateSchedule(item, now)
	if err != nil {
		return Schedule{}, err
	}
	item.ID, err = newID("sch")
	if err != nil {
		return Schedule{}, err
	}
	item.Enabled = true
	item.CreatedAt = now.UTC().Format(timestampLayout)
	item.UpdatedAt = item.CreatedAt
	_, err = tx.Exec(`INSERT INTO schedule (`+scheduleColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.ID, item.RoomID, item.ProjectID, item.CreatorKind, item.CreatorID, nullable(item.TargetAgentID), item.TargetLead, item.Body, item.Kind, item.Timezone, item.IntervalSeconds, item.LocalTime, item.Weekday, item.Enabled, item.NextRunAt, "", "", nil, 0, item.CreatedAt, item.UpdatedAt)
	if err != nil {
		return Schedule{}, err
	}
	creator, err := tx.scheduleCreator(item)
	if err != nil {
		return Schedule{}, err
	}
	if _, err = tx.InsertMessage(Message{RoomID: item.RoomID, AuthorKind: "system", AuthorID: "system", Body: fmt.Sprintf("%s scheduled a note for %s (next %s, %s)", creator, scheduleTarget(item), item.NextRunAt, item.Timezone)}); err != nil {
		return Schedule{}, err
	}
	return item, nil
}
func scheduleTarget(s Schedule) string {
	if s.TargetLead {
		return "the room lead"
	}
	return s.TargetAgentID
}

func (tx *Tx) scheduleCreator(s Schedule) (string, error) {
	var display string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE((SELECT login FROM user WHERE id=?),(SELECT slug FROM agent WHERE id=?),?)`, s.CreatorID, s.CreatorID, s.CreatorID).Scan(&display)
	return display, err
}

func (tx *Tx) SetScheduleEnabled(id, roomID string, enabled bool, rev int64, now time.Time) (Schedule, error) {
	s, err := scanSchedule(tx.tx.QueryRowContext(tx.ctx, `SELECT `+scheduleColumns+` FROM schedule WHERE id=? AND room_id=?`, id, roomID))
	if err != nil {
		return Schedule{}, err
	}
	if s.Rev != rev {
		return Schedule{}, ErrStaleRev
	}
	if s.Enabled == enabled {
		return s, nil
	}
	if enabled && !s.Enabled {
		var kind, lifecycle string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT kind,lifecycle FROM room WHERE id=?`, roomID).Scan(&kind, &lifecycle); err != nil {
			return Schedule{}, err
		}
		if kind != "task" || lifecycle != "open" || s.Kind == "once" && s.LastOutcome != "" {
			return Schedule{}, ErrScheduleInvalid
		}
		var count int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT count(*) FROM schedule WHERE room_id=? AND enabled=1`, roomID).Scan(&count); err != nil {
			return Schedule{}, err
		}
		if count >= 20 {
			return Schedule{}, ErrScheduleLimit
		}
		at, err := NextScheduleTime(s, now)
		if err != nil {
			return Schedule{}, err
		}
		s.NextRunAt = at.UTC().Format(timestampLayout)
	}
	s.Enabled = enabled
	s.Rev++
	s.UpdatedAt = now.UTC().Format(timestampLayout)
	if _, err := tx.Exec(`UPDATE schedule SET enabled=?,rev=?,next_run_at=?,updated_at=? WHERE id=?`, s.Enabled, s.Rev, s.NextRunAt, s.UpdatedAt, s.ID); err != nil {
		return Schedule{}, err
	}
	label := "paused"
	if enabled {
		label = "resumed"
	}
	_, err = tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: fmt.Sprintf("Schedule %s %s", s.ID, label)})
	return s, err
}
func (tx *Tx) DeleteSchedule(id, roomID string) error {
	result, err := tx.Exec(`DELETE FROM schedule WHERE id=? AND room_id=?`, id, roomID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	_, err = tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: "Schedule " + id + " deleted"})
	return err
}

// AdvanceSchedules is a serial, transactional catch-up pass. One due instant
// is processed per schedule, then next_run_at leaps to the future.
func (s *Store) AdvanceSchedules(ctx context.Context, now time.Time) (int, error) {
	processed := 0
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id FROM schedule WHERE enabled=1 AND next_run_at<=? ORDER BY next_run_at,id LIMIT 100`, now.UTC().Format(timestampLayout))
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := tx.advanceSchedule(id, now); err != nil {
				return err
			}
			processed++
		}
		return nil
	})
	return processed, err
}
func (tx *Tx) advanceSchedule(id string, now time.Time) error {
	s, err := scanSchedule(tx.tx.QueryRowContext(tx.ctx, `SELECT `+scheduleColumns+` FROM schedule WHERE id=?`, id))
	if err != nil {
		return err
	}
	var roomLifecycle, roomGate, projectGate, roomDesired, projectDesired string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.lifecycle,r.gate,p.gate,r.control_desired,p.control_desired FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`, s.RoomID).Scan(&roomLifecycle, &roomGate, &projectGate, &roomDesired, &projectDesired); err != nil {
		return err
	}
	reason := ""
	target := s.TargetAgentID
	switch {
	case roomLifecycle != "open":
		reason = "room_closed"
	case roomGate != "open" || roomDesired != "active":
		reason = "room_paused_or_limited"
	case projectGate != "open" || projectDesired != "active":
		reason = "project_paused_or_limited"
	}
	if reason == "" && s.TargetLead {
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.id FROM agent a JOIN role role ON role.id=a.role_id JOIN room_member rm ON rm.member_id=a.id AND rm.member_kind='agent' AND rm.left_at IS NULL WHERE rm.room_id=? AND role.cap_is_lead=1 AND a.revoked_at IS NULL ORDER BY a.id LIMIT 1`, s.RoomID).Scan(&target)
		if errors.Is(err, sql.ErrNoRows) {
			reason = "lead_not_in_room"
		} else if err != nil {
			return err
		}
	}
	if reason == "" {
		var eligible bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member rm JOIN agent a ON a.id=rm.member_id WHERE rm.room_id=? AND rm.member_kind='agent' AND rm.member_id=? AND rm.left_at IS NULL AND a.revoked_at IS NULL AND a.desired='active')`, s.RoomID, target).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			reason = "target_unavailable_or_left"
		}
	}
	var messageID any
	outcome := "delivered"
	if reason != "" {
		outcome = "skipped"
	}
	if outcome == "delivered" || s.LastOutcome != outcome || s.LastReason != reason {
		creator, err := tx.scheduleCreator(s)
		if err != nil {
			return err
		}
		body := fmt.Sprintf("Scheduled by %s for %s; due %s; processed %s. ", creator, scheduleTarget(s), s.NextRunAt, now.UTC().Format(timestampLayout))
		msg := Message{RoomID: s.RoomID, AuthorKind: "system", AuthorID: "system"}
		if outcome == "delivered" {
			msg.Body = body + s.Body
			msg.OnlyTargets = []string{target}
		} else {
			msg.Body = body + "Skipped: " + reason
		}
		sent, err := tx.InsertMessage(msg)
		if err != nil {
			return err
		}
		messageID = sent.ID
	}
	if _, err := tx.Exec(`INSERT INTO schedule_run(schedule_id,due_at,processed_at,outcome,reason,message_id) VALUES (?,?,?,?,?,?)`, s.ID, s.NextRunAt, now.UTC().Format(timestampLayout), outcome, reason, messageID); err != nil {
		return err
	}
	enabled := s.Kind != "once" && roomLifecycle == "open"
	next := s.NextRunAt
	if enabled {
		future, err := NextScheduleTime(s, now)
		if err != nil {
			return err
		}
		next = future.UTC().Format(timestampLayout)
	}
	_, err = tx.Exec(`UPDATE schedule SET enabled=?,next_run_at=?,last_outcome=?,last_reason=?,last_run_at=?,updated_at=? WHERE id=?`, enabled, next, outcome, reason, now.UTC().Format(timestampLayout), now.UTC().Format(timestampLayout), id)
	return err
}
