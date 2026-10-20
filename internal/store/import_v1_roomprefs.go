package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/goccy/go-json"
)

// v1RoomPref is one row of v1's room_prefs: a person's hidden and pinned flags for a room.
type v1RoomPref struct {
	Agent  string
	Room   string
	Hidden bool
	Pinned bool
}

// loadV1RoomPrefs reads room_prefs. A backup without the table has no preferences (v1 creates it
// with the other tables, but an older backup may predate it).
func loadV1RoomPrefs(ctx context.Context, db *sql.DB) ([]v1RoomPref, error) {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='room_prefs'`).Scan(&present); err != nil || present == 0 {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT agent_id,room_id,hidden,pinned FROM room_prefs ORDER BY agent_id,room_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prefs []v1RoomPref
	for rows.Next() {
		var pref v1RoomPref
		var hidden, pinned int
		if err := rows.Scan(&pref.Agent, &pref.Room, &hidden, &pinned); err != nil {
			return nil, err
		}
		pref.Hidden, pref.Pinned = hidden != 0, pinned != 0
		prefs = append(prefs, pref)
	}
	return prefs, rows.Err()
}

// importV1RoomPrefs maps each mapped human's hidden and pinned rooms. A hidden room becomes a room_view_preference. v2 has no pins:
// a pinned room puts the project it landed in at the front of that person's manual project order (the sidebar_project_order
// preference), but only when they do not have an order of their own yet (a pinned direct message has no project in the tree and
// is skipped). Only humans' rows count (an agent has no sidebar), and a row with neither flag set carries nothing. Two v1 people
// mapped to one login merge their flags. An existing v2 preference always wins, so a rerun, or a choice the person made in v2
// since, is never overwritten.
func importV1RoomPrefs(ctx context.Context, tx *sql.Tx, data *v1ImportData, mapping V1ImportMap, roomIDs, userIDs map[string]string, report *V1ImportReport) error {
	type key struct{ user, room string }
	type flags struct{ hidden, pinned bool }
	merged := map[key]flags{}
	var order []key
	for _, pref := range data.RoomPrefs {
		if (!pref.Hidden && !pref.Pinned) || v1ImportKind(pref.Agent, data.Agents) != "user" {
			continue
		}
		user, room := userIDs[pref.Agent], roomIDs[pref.Room]
		if skippedV1Human(mapping, pref.Agent) || skippedV1Room(mapping, pref.Room) || user == "" || room == "" {
			report.Skipped["room_prefs"]++
			continue
		}
		k := key{user, room}
		if _, seen := merged[k]; !seen {
			order = append(order, k)
		}
		current := merged[k]
		current.hidden, current.pinned = current.hidden || pref.Hidden, current.pinned || pref.Pinned
		merged[k] = current
	}
	hiddenInserted := map[key]bool{}
	pinnedProjects := map[string]map[string]bool{} // user -> the projects of their pinned rooms
	pinnedKind := map[key]string{}                 // "project" or "dm" for each pinned (user, room)
	for _, k := range order {
		pref := merged[k]
		if pref.hidden {
			result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO room_view_preference(user_id,room_id,hidden) VALUES (?,?,1)`, k.user, k.room)
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n > 0 {
				hiddenInserted[k] = true
			}
		}
		if pref.pinned {
			var projectID, kind string
			if err := tx.QueryRowContext(ctx, `SELECT project_id,kind FROM room WHERE id=?`, k.room).Scan(&projectID, &kind); err != nil {
				return err
			}
			if kind == "dm" {
				pinnedKind[k] = "dm"
				continue
			}
			pinnedKind[k] = "project"
			if pinnedProjects[k.user] == nil {
				pinnedProjects[k.user] = map[string]bool{}
			}
			pinnedProjects[k.user][projectID] = true
		}
	}
	// A person without a manual order gets one that starts with their pinned projects (oldest first); one who has an order keeps it.
	orderWritten := map[string]bool{}
	users := make([]string, 0, len(pinnedProjects))
	for user := range pinnedProjects {
		users = append(users, user)
	}
	sort.Strings(users)
	for _, user := range users {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT value FROM user_preference WHERE user_id=? AND key='sidebar_project_order'`, user).Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ids := make([]string, 0, len(pinnedProjects[user]))
		for id := range pinnedProjects[user] {
			ids = append(ids, id)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM project WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`) ORDER BY created_at,id`, anyStrings(ids)...)
		if err != nil {
			return err
		}
		var ordered []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ordered = append(ordered, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(ordered)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_preference(user_id,key,value,updated_at) VALUES (?,'sidebar_project_order',?,?)`, user, string(encoded), timestamp()); err != nil {
			return err
		}
		orderWritten[user] = true
	}
	for _, k := range order {
		pref := merged[k]
		inserted := hiddenInserted[k] || pref.pinned && pinnedKind[k] == "project" && orderWritten[k.user]
		switch {
		case pref.pinned && pinnedKind[k] == "dm" && !pref.hidden:
			report.Skipped["room_prefs"]++
		case inserted:
			report.Imported["room_prefs"]++
		default:
			report.AlreadyPresent["room_prefs"]++
		}
	}
	return nil
}

func anyStrings(list []string) []any {
	out := make([]any, len(list))
	for i, item := range list {
		out[i] = item
	}
	return out
}
