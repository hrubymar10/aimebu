package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// Daily limits belong to a project (all its rooms together). A project either inherits the global default or sets its own value or none,
// separately for cost and turns. DailyCostCap and DailyTurnCap are the caps in force (the project's own, none, or the inherited default,
// then raised for today by a "continue"); CostMode/TurnMode say which, and Inherited* is the global default, for "Inherit (global: X)".
type UsageLimit struct {
	CostMode               string   `json:"cost_mode"`
	TurnMode               string   `json:"turn_mode"`
	InheritedCostCap       *float64 `json:"inherited_cost_cap"`
	InheritedTurnCap       *int64   `json:"inherited_turn_cap"`
	DailyCostCap           *float64 `json:"daily_cost_cap"`
	DailyTurnCap           *int64   `json:"daily_turn_cap"`
	DailyCost              float64  `json:"daily_cost"`
	DailyTurns             int64    `json:"daily_turns"`
	CostCoverage           string   `json:"cost_coverage"`
	TurnsWithEstimatedCost int64    `json:"turns_with_estimated_cost"`
	DayStart               string   `json:"day_start"`
	TZ                     string   `json:"tz"`
	Desired                string   `json:"desired"`
	EffectiveState         string   `json:"effective_state"`
	Rev                    int64    `json:"rev"`
}

func localUsageDay(now time.Time) (start, end time.Time, tz string) {
	local := now.In(time.Local)
	y, m, d := local.Date()
	start = time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	end = start.AddDate(0, 0, 1)
	tz = time.Local.String()
	if tz == "Local" {
		tz, _ = start.Zone()
	}
	return start, end, tz
}

func (tx *Tx) budgetPaused(kind, id string, now time.Time) (bool, error) {
	start, _, _ := localUsageDay(now)
	var paused bool
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE((SELECT paused FROM usage_limit_day
		WHERE scope_kind=? AND scope_id=? AND day_start=?),0)`, kind, id,
		start.UTC().Format(timestampLayout)).Scan(&paused)
	return paused, err
}

func scopeProjectAndRoom(tx *Tx, kind, id string) (projectID, roomID string, err error) {
	if _, err = scopeTable(kind); err != nil {
		return "", "", err
	}
	if kind == "room" {
		roomID = id
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id=?`, id).Scan(&projectID)
	} else {
		projectID = id
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? ORDER BY kind='home' DESC,created_at,id LIMIT 1`, id).Scan(&roomID)
	}
	return
}

func (tx *Tx) usageLimit(kind, id string, now time.Time) (UsageLimit, error) {
	var err error
	if _, err := scopeTable(kind); err != nil {
		return UsageLimit{}, err
	}
	var exists string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM `+kind+` WHERE id=?`, id).Scan(&exists); err != nil {
		return UsageLimit{}, err
	}
	start, end, tz := localUsageDay(now)
	startUTC, endUTC := start.UTC().Format(timestampLayout), end.UTC().Format(timestampLayout)
	out := UsageLimit{DayStart: start.Format(time.RFC3339), TZ: tz, CostCoverage: "unknown"}
	var cost sql.NullFloat64
	var turns sql.NullInt64
	out.CostMode, out.TurnMode = "inherit", "inherit"
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT daily_cost_cap,daily_turn_cap,rev,cost_mode,turn_mode FROM usage_limit WHERE scope_kind=? AND scope_id=?`, kind, id).
		Scan(&cost, &turns, &out.Rev, &out.CostMode, &out.TurnMode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UsageLimit{}, err
	}
	defaultCost, defaultTurns, err := tx.usageLimitDefault()
	if err != nil {
		return UsageLimit{}, err
	}
	out.InheritedCostCap, out.InheritedTurnCap = defaultCost, defaultTurns
	switch out.CostMode {
	case "custom":
		if cost.Valid {
			out.DailyCostCap = &cost.Float64
		}
	case "inherit":
		out.DailyCostCap = defaultCost
	}
	switch out.TurnMode {
	case "custom":
		if turns.Valid {
			out.DailyTurnCap = &turns.Int64
		}
	case "inherit":
		out.DailyTurnCap = defaultTurns
	}
	where, turnWhere := "room_id=?", "t.room_id=?"
	if kind == "project" {
		where, turnWhere = "project_id=?", "(SELECT a.project_id FROM agent a WHERE a.id=t.agent_id)=?"
	}
	var knownTurns int64
	if err = tx.tx.QueryRowContext(tx.ctx, usageLimitTurnsSQL(turnWhere), id, startUTC, endUTC).
		Scan(&out.DailyTurns, &knownTurns, &out.TurnsWithEstimatedCost); err != nil {
		return UsageLimit{}, err
	}
	if out.DailyTurns > 0 {
		out.CostCoverage = "partial"
		if knownTurns == out.DailyTurns {
			out.CostCoverage = "known"
			if out.TurnsWithEstimatedCost > 0 {
				out.CostCoverage = "estimated"
			}
		} else if knownTurns == 0 {
			out.CostCoverage = "unknown"
		}
	}
	if err = tx.tx.QueryRowContext(tx.ctx, usageLimitCostSQL(where), id, startUTC, endUTC).
		Scan(&out.DailyCost); err != nil {
		return UsageLimit{}, err
	}
	var desired, gate string
	if err = tx.tx.QueryRowContext(tx.ctx, `SELECT control_desired,gate FROM `+kind+` WHERE id=?`, id).Scan(&desired, &gate); err != nil {
		return UsageLimit{}, err
	}
	out.Desired, out.EffectiveState = desired, desired
	if gate != "open" && desired == "active" {
		out.EffectiveState = "paused"
	}
	var overrideCost sql.NullFloat64
	var overrideTurns sql.NullInt64
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT override_cost,override_turns FROM usage_limit_day
		WHERE scope_kind=? AND scope_id=? AND day_start=?`, kind, id, startUTC).Scan(&overrideCost, &overrideTurns)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UsageLimit{}, err
	}
	if overrideCost.Valid {
		out.DailyCostCap = &overrideCost.Float64
	}
	if overrideTurns.Valid {
		out.DailyTurnCap = &overrideTurns.Int64
	}
	return out, nil
}

const (
	usageDefaultCostKey  = "usage_limit.default_cost"
	usageDefaultTurnsKey = "usage_limit.default_turns"
)

// usageLimitDefault is the global default projects inherit; nil means no limit.
func (tx *Tx) usageLimitDefault() (*float64, *int64, error) {
	var cost *float64
	var turns *int64
	var value string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT value FROM global_setting WHERE key=?`, usageDefaultCostKey).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	if err == nil {
		if n, convErr := strconv.ParseFloat(value, 64); convErr == nil && n > 0 {
			cost = &n
		}
	}
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT value FROM global_setting WHERE key=?`, usageDefaultTurnsKey).Scan(&value)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	if err == nil {
		if n, convErr := strconv.ParseInt(value, 10, 64); convErr == nil && n > 0 {
			turns = &n
		}
	}
	return cost, turns, nil
}

// UsageLimitDefault is the global default for daily limits.
type UsageLimitDefault struct {
	DailyCostCap *float64 `json:"daily_cost_cap"`
	DailyTurnCap *int64   `json:"daily_turn_cap"`
}

func (s *Store) UsageLimitDefault(ctx context.Context) (UsageLimitDefault, error) {
	var out UsageLimitDefault
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		out.DailyCostCap, out.DailyTurnCap, err = tx.usageLimitDefault()
		return err
	})
	return out, err
}

func sameCap(a, b *float64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func sameTurns(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func validCostCap(cost *float64) bool {
	return cost == nil || (*cost > 0 && !math.IsNaN(*cost) && !math.IsInf(*cost, 0))
}
func validTurnCap(turns *int64) bool { return turns == nil || *turns > 0 }

// SetUsageLimitDefault stores the global default (nil clears a field: no limit) and re-checks every project that inherits it, so a
// raised default lifts a pause the old one caused and a lowered one takes effect without waiting for the next turn.
func (s *Store) SetUsageLimitDefault(ctx context.Context, actor string, cost *float64, turns *int64) (UsageLimitDefault, error) {
	if !validCostCap(cost) || !validTurnCap(turns) {
		return UsageLimitDefault{}, ErrInvalidTransition
	}
	err := s.Tx(ctx, func(tx *Tx) error {
		if err := tx.advanceUsageDays(time.Now()); err != nil {
			return err
		}
		oldCost, oldTurns, err := tx.usageLimitDefault()
		if err != nil {
			return err
		}
		costChanged, turnsChanged := !sameCap(oldCost, cost), !sameTurns(oldTurns, turns)
		if !costChanged && !turnsChanged {
			return nil // saving what is already the default changes nothing: no overrides cleared, no project re-checked
		}
		for key, value := range map[string]any{usageDefaultCostKey: cost, usageDefaultTurnsKey: turns} {
			text := ""
			switch v := value.(type) {
			case *float64:
				if v != nil {
					text = strconv.FormatFloat(*v, 'f', -1, 64)
				}
			case *int64:
				if v != nil {
					text = strconv.FormatInt(*v, 10)
				}
			}
			if text == "" {
				if _, err := tx.Exec(`DELETE FROM global_setting WHERE key=?`, key); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(`INSERT INTO global_setting(key,value,updated_at) VALUES (?,?,?)
				ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, text, timestamp()); err != nil {
				return err
			}
		}
		if _, err := tx.AppendEvent(Event{Kind: "usage_limit_default_updated", PayloadJSON: payloadJSON(map[string]string{"actor_id": actor})}); err != nil {
			return err
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT p.id, COALESCE(u.cost_mode,'inherit'), COALESCE(u.turn_mode,'inherit') FROM project p
			LEFT JOIN usage_limit u ON u.scope_kind='project' AND u.scope_id=p.id
			WHERE p.system=0 AND (u.scope_id IS NULL OR u.cost_mode='inherit' OR u.turn_mode='inherit') ORDER BY p.id`)
		if err != nil {
			return err
		}
		type inheriting struct {
			id                        string
			inheritCost, inheritTurns bool
		}
		var projects []inheriting
		for rows.Next() {
			var p inheriting
			var costMode, turnMode string
			if err := rows.Scan(&p.id, &costMode, &turnMode); err != nil {
				rows.Close()
				return err
			}
			p.inheritCost, p.inheritTurns = costMode == "inherit", turnMode == "inherit"
			projects = append(projects, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		start, _, _ := localUsageDay(time.Now())
		dayKey := start.UTC().Format(timestampLayout)
		for _, p := range projects {
			// Today's "Continue" raised the cap in force. A new default is a new cap for the field the project inherits, so that raise
			// no longer applies to it; a field the project sets itself, or whose default did not change, keeps its raise.
			if p.inheritCost && costChanged {
				if _, err := tx.Exec(`UPDATE usage_limit_day SET override_cost=NULL WHERE scope_kind='project' AND scope_id=? AND day_start=?`, p.id, dayKey); err != nil {
					return err
				}
			}
			if p.inheritTurns && turnsChanged {
				if _, err := tx.Exec(`UPDATE usage_limit_day SET override_turns=NULL WHERE scope_kind='project' AND scope_id=? AND day_start=?`, p.id, dayKey); err != nil {
					return err
				}
			}
			if (p.inheritCost && costChanged) || (p.inheritTurns && turnsChanged) {
				if err := tx.reconcileUsageLimit("project", p.id, actor); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return UsageLimitDefault{}, err
	}
	return s.UsageLimitDefault(ctx)
}

// UsageLimit is a project's daily usage and limits.
func (s *Store) UsageLimit(ctx context.Context, projectID string) (UsageLimit, error) {
	var out UsageLimit
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		if err = tx.advanceUsageDays(time.Now()); err != nil {
			return err
		}
		out, err = tx.usageLimit("project", projectID, time.Now())
		return err
	})
	return out, err
}

// UsageLimitInput is what a project's limits are set to: for each of cost and turns, inherit the global default, a custom value, or none.
type UsageLimitInput struct {
	CostMode string // "inherit", "custom" or "none"
	Cost     *float64
	TurnMode string
	Turns    *int64
}

func (tx *Tx) UpdateUsageLimit(projectID, actor string, in UsageLimitInput, rev int64) (UsageLimit, error) {
	if err := tx.advanceUsageDays(time.Now()); err != nil {
		return UsageLimit{}, err
	}
	if err := tx.roomLifecyclePermission(projectID, actor); err != nil {
		return UsageLimit{}, err
	}
	modeOK := func(mode string) bool { return mode == "inherit" || mode == "custom" || mode == "none" }
	if !modeOK(in.CostMode) || !modeOK(in.TurnMode) || !validCostCap(in.Cost) || !validTurnCap(in.Turns) ||
		(in.CostMode == "custom") != (in.Cost != nil) || (in.TurnMode == "custom") != (in.Turns != nil) {
		return UsageLimit{}, ErrInvalidTransition
	}
	var oldRev int64
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT rev FROM usage_limit WHERE scope_kind='project' AND scope_id=?`, projectID).Scan(&oldRev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UsageLimit{}, err
	}
	if rev != oldRev {
		return UsageLimit{}, ErrStaleRev
	}
	if _, err := tx.Exec(`INSERT INTO usage_limit(scope_kind,scope_id,daily_cost_cap,daily_turn_cap,rev,cost_mode,turn_mode)
		VALUES ('project',?,?,?,?,?,?) ON CONFLICT(scope_kind,scope_id) DO UPDATE SET
		daily_cost_cap=excluded.daily_cost_cap,daily_turn_cap=excluded.daily_turn_cap,rev=excluded.rev,
		cost_mode=excluded.cost_mode,turn_mode=excluded.turn_mode`,
		projectID, in.Cost, in.Turns, oldRev+1, in.CostMode, in.TurnMode); err != nil {
		return UsageLimit{}, err
	}
	if _, err := tx.AppendEvent(Event{Kind: "usage_limit_updated", ProjectID: projectID,
		PayloadJSON: payloadJSON(map[string]string{"scope_kind": "project", "scope_id": projectID, "actor_id": actor})}); err != nil {
		return UsageLimit{}, err
	}
	start, _, _ := localUsageDay(time.Now())
	if _, err := tx.Exec(`UPDATE usage_limit_day SET override_cost=NULL,override_turns=NULL
		WHERE scope_kind='project' AND scope_id=? AND day_start=?`, projectID, start.UTC().Format(timestampLayout)); err != nil {
		return UsageLimit{}, err
	}
	if err := tx.reconcileUsageLimit("project", projectID, actor); err != nil {
		return UsageLimit{}, err
	}
	return tx.usageLimit("project", projectID, time.Now())
}

// reconcileUsageLimit applies a changed cap: a scope that the budget paused and that is now under its cap is released (gate reopened,
// the open notice resolved), and anything over a cap is evaluated as usual.
func (tx *Tx) reconcileUsageLimit(kind, id, actor string) error {
	start, _, _ := localUsageDay(time.Now())
	dayKey := start.UTC().Format(timestampLayout)
	current, err := tx.usageLimit(kind, id, time.Now())
	if err != nil {
		return err
	}
	ratio := 0.0
	if current.DailyCostCap != nil {
		ratio = math.Max(ratio, current.DailyCost / *current.DailyCostCap)
	}
	if current.DailyTurnCap != nil {
		ratio = math.Max(ratio, float64(current.DailyTurns)/float64(*current.DailyTurnCap))
	}
	if paused, err := tx.budgetPaused(kind, id, time.Now()); err != nil {
		return err
	} else if paused && ratio < 1 {
		if _, err := tx.Exec(`UPDATE usage_limit_day SET paused=0,limited=0,abandoned=0
			WHERE scope_kind=? AND scope_id=? AND day_start=?`, kind, id, dayKey); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE `+kind+` SET gate=CASE WHEN control_desired='active' THEN 'open' ELSE 'paused' END,
			gate_rev=gate_rev+1 WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=?
			WHERE requester_kind='system' AND requester_id=? AND kind='usage_limit' AND state='open'`,
			actor, timestamp(), kind+":"+id+":"+dayKey); err != nil {
			return err
		}
	}
	return tx.evaluateUsageLimit(kind, id, time.Now())
}

// The two reads behind a scope's daily figures: its turns (and how many have a cost) and its summed cost, both over a time range. The
// scope is a room or a project; where and turnWhere name its column. They are functions so a test can ask SQLite how it runs them.
func usageLimitTurnsSQL(turnWhere string) string {
	return `SELECT COUNT(*),COALESCE(SUM(CASE WHEN EXISTS(
		SELECT 1 FROM usage_record u WHERE u.turn_id=t.id AND u.cost_est IS NOT NULL) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN EXISTS(SELECT 1 FROM usage_record u WHERE u.turn_id=t.id AND u.cost_est IS NOT NULL AND u.source='estimated') THEN 1 ELSE 0 END),0)
		FROM turn t JOIN room r ON r.id=t.room_id WHERE ` + turnWhere + ` AND t.started_at>=? AND t.started_at<?`
}

func usageLimitCostSQL(where string) string {
	return `SELECT COALESCE(SUM(cost_est),0) FROM usage_record WHERE ` + where + ` AND created_at>=? AND created_at<?`
}

func (tx *Tx) evaluateUsageFor(roomID, projectID string) error {
	now := time.Now()
	if err := tx.advanceUsageDays(now); err != nil {
		return err
	}
	// Limits are per project: the whole project's spend, all its rooms together, is what is checked.
	return tx.evaluateUsageLimit("project", projectID, now)
}

// AdvanceUsageDays removes only the budget-owned gate on the next local day.
// Manual scope pauses are retained because control_desired is independent.
func (s *Store) AdvanceUsageDays(ctx context.Context) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.advanceUsageDays(time.Now()) })
}

func (tx *Tx) advanceUsageDays(now time.Time) error {
	start, _, _ := localUsageDay(now)
	dayKey := start.UTC().Format(timestampLayout)
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT scope_kind,scope_id,day_start,paused FROM usage_limit_day
		WHERE day_start<? AND rolled_over=0 ORDER BY day_start,scope_kind,scope_id`, dayKey)
	if err != nil {
		return err
	}
	type oldDay struct {
		kind, id, day string
		paused        bool
	}
	var days []oldDay
	for rows.Next() {
		var day oldDay
		if err := rows.Scan(&day.kind, &day.id, &day.day, &day.paused); err != nil {
			rows.Close()
			return err
		}
		days = append(days, day)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, day := range days {
		if day.paused {
			projectID, roomID, err := scopeProjectAndRoom(tx, day.kind, day.id)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE `+day.kind+` SET gate=CASE WHEN control_desired='active' THEN 'open' ELSE 'paused' END,
				gate_rev=gate_rev+1 WHERE id=?`, day.id); err != nil {
				return err
			}
			if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
				Body: fmt.Sprintf("%s daily usage reset; its previous control state is restored", day.kind)}); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "usage_limit_reset", ProjectID: projectID, RoomID: roomID,
				PayloadJSON: payloadJSON(map[string]string{"scope_kind": day.kind, "scope_id": day.id})}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_at=?
			WHERE requester_kind='system' AND requester_id=? AND kind IN ('usage_info','usage_limit') AND state='open'`,
			timestamp(), day.kind+":"+day.id+":"+day.day); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE usage_limit_day SET rolled_over=1,paused=0 WHERE scope_kind=? AND scope_id=? AND day_start=?`,
			day.kind, day.id, day.day); err != nil {
			return err
		}
	}
	return nil
}

func (tx *Tx) evaluateUsageLimit(kind, id string, now time.Time) error {
	limit, err := tx.usageLimit(kind, id, now)
	if err != nil {
		return err
	}
	if limit.DailyCostCap == nil && limit.DailyTurnCap == nil {
		return nil
	}
	start, _, tz := localUsageDay(now)
	dayKey := start.UTC().Format(timestampLayout)
	if _, err := tx.Exec(`INSERT OR IGNORE INTO usage_limit_day(scope_kind,scope_id,day_start,tz)
		VALUES (?,?,?,?)`, kind, id, dayKey, tz); err != nil {
		return err
	}
	var warned, limited, paused, continued, abandoned bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT warned,limited,paused,continued,abandoned FROM usage_limit_day
		WHERE scope_kind=? AND scope_id=? AND day_start=?`, kind, id, dayKey).
		Scan(&warned, &limited, &paused, &continued, &abandoned); err != nil {
		return err
	}
	if abandoned {
		return nil
	}
	ratio := 0.0
	if limit.DailyCostCap != nil {
		ratio = math.Max(ratio, limit.DailyCost / *limit.DailyCostCap)
	}
	if limit.DailyTurnCap != nil {
		ratio = math.Max(ratio, float64(limit.DailyTurns)/float64(*limit.DailyTurnCap))
	}
	if ratio < .8 || warned && ratio < 1 || limited {
		return nil
	}
	projectID, noticeRoom, err := scopeProjectAndRoom(tx, kind, id)
	if err != nil {
		return err
	}
	itemKind := "usage_info"
	message := fmt.Sprintf("%s daily usage reached 80%% of its limit", kind)
	if ratio >= 1 {
		itemKind = "usage_limit"
		message = fmt.Sprintf("%s daily usage reached its limit; work is paused until review or the next local day", kind)
	}
	if ratio < 1 && warned {
		return nil
	}
	if _, err := tx.InsertMessage(Message{RoomID: noticeRoom, AuthorKind: "system", AuthorID: "system", Body: message}); err != nil {
		return err
	}
	options := inboxAsk{Text: message}
	if itemKind == "usage_limit" {
		options.Options = []string{"Abandon: stay paused until tomorrow"}
		if !continued {
			options.Options = append([]string{continueLabel(limit)}, options.Options...)
		}
	}
	itemID, err := newID("inb")
	if err != nil {
		return err
	}
	optionsJSON, err := json.Marshal(options)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES (?,?,?,'system',?,?,?,?)`, itemID, projectID, noticeRoom,
		kind+":"+id+":"+dayKey, itemKind, string(optionsJSON), timestamp())
	if err != nil {
		return err
	}
	if itemKind == "usage_limit" {
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_at=?
			WHERE requester_kind='system' AND requester_id=? AND kind='usage_info' AND state='open'`,
			timestamp(), kind+":"+id+":"+dayKey); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE usage_limit_day SET warned=1,limited=1,paused=1 WHERE scope_kind=? AND scope_id=? AND day_start=?`, kind, id, dayKey); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE `+kind+` SET gate='paused',gate_rev=gate_rev+1 WHERE id=?`, id); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`UPDATE usage_limit_day SET warned=1 WHERE scope_kind=? AND scope_id=? AND day_start=?`, kind, id, dayKey); err != nil {
			return err
		}
	}
	_, err = tx.AppendEvent(Event{Kind: "usage_limit_threshold", ProjectID: projectID, RoomID: noticeRoom,
		PayloadJSON: payloadJSON(map[string]string{"scope_kind": kind, "scope_id": id, "ratio": fmt.Sprint(ratio), "item_id": itemID})})
	return err
}

func continueLabel(limit UsageLimit) string {
	parts := []string{}
	if limit.DailyCostCap != nil {
		value := *limit.DailyCostCap
		if limit.DailyCost >= value {
			value = math.Max(value*2, limit.DailyCost*2)
		}
		parts = append(parts, fmt.Sprintf("$%.2f", value))
	}
	if limit.DailyTurnCap != nil {
		value := *limit.DailyTurnCap
		if limit.DailyTurns >= value {
			value = max(value*2, limit.DailyTurns*2)
		}
		parts = append(parts, fmt.Sprintf("%d turns", value))
	}
	return "Continue: raise today's cap to " + strings.Join(parts, " / ")
}
