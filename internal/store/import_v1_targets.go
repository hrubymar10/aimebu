package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Where a v1 room goes in v2 (V1ImportRoom.Target):
//
//	"room" (or empty)  a separate room under the project's home room, as before;
//	"root"             the project's home room itself, the v1 messages joining whatever is there;
//	"sub"              a sub-room of the home room with a chosen title (default "general"); every v1 room that names the same project and
//	                   title shares that one room.
//
// A v1 DM always becomes a DM. Messages keep their v1 numbers and times, so rooms merged into one target interleave by number, which in
// v1 is time. Each v1 room is recorded in the ledger against the room it landed in, and a shared sub-room is recorded once under
// "target:<project>:<title>", so a rerun adds nothing twice. The ledger holds one row per kind and new id, so a v1 room placed in a shared
// room is recorded as kind "room_in" with the new id "<target room>|<v1 room>".
// V1SystemRoom is v1's own event room. It is never imported: the preview skips it, the importer ignores any mapping for it.
const V1SystemRoom = "_system"

const (
	V1TargetRoom = "room"
	V1TargetRoot = "root"
	V1TargetSub  = "sub"
	// V1DefaultSubRoomTitle is the title a sub-room target gets when the owner does not choose one.
	V1DefaultSubRoomTitle = GeneralRoomTitle
	maxV1SubRoomTitle     = 80
)

func v1RoomTarget(spec V1ImportRoom, roomID string) string {
	if isV1DM(roomID) || spec.Target == "" {
		return V1TargetRoom
	}
	return spec.Target
}

func v1SubRoomTitle(spec V1ImportRoom) string {
	if title := strings.TrimSpace(spec.Title); title != "" {
		return title
	}
	return V1DefaultSubRoomTitle
}

// validateV1RoomTarget refuses a target the importer would not understand, before anything is written.
func validateV1RoomTarget(roomID string, spec V1ImportRoom) error {
	switch spec.Target {
	case "", V1TargetRoom:
		return nil
	case V1TargetRoot, V1TargetSub:
		if isV1DM(roomID) {
			return fmt.Errorf("direct message %q cannot be placed in a project room", roomID)
		}
	default:
		return fmt.Errorf("room %q has unknown target %q", roomID, spec.Target)
	}
	if spec.Target == V1TargetSub {
		title := v1SubRoomTitle(spec)
		if utf8.RuneCountInString(title) > maxV1SubRoomTitle || strings.ContainsAny(title, "\r\n\t") {
			return fmt.Errorf("room %q sub-room title must be one line of at most %d characters", roomID, maxV1SubRoomTitle)
		}
	}
	return nil
}

// v1SubRoomKey is the import ledger key of a shared sub-room. The title can hold ':' (and so can a project alias), so both parts are
// quoted: ("a", "b:c") and ("a:b", "c") are two keys.
func v1SubRoomKey(alias, title string) string {
	return "target:" + strconv.Quote(alias) + ":" + strconv.Quote(title)
}

// v1HomeRoom is the project's home room: the one this import made, or the project's existing one.
func v1HomeRoom(ctx context.Context, tx *sql.Tx, sourceKey, alias, projectID string) (string, error) {
	key := "home:" + alias
	home, present, err := importMapID(ctx, tx, sourceKey, "room", key)
	if err != nil {
		return "", err
	}
	if present {
		return home, nil
	}
	if err := tx.QueryRowContext(ctx, "SELECT id FROM room WHERE project_id=? AND kind='home' ORDER BY created_at LIMIT 1", projectID).Scan(&home); err != nil {
		return "", fmt.Errorf("project %q has no home room: %w", alias, err)
	}
	if err := addImportMap(ctx, tx, sourceKey, "room", key, home); err != nil {
		return "", err
	}
	return home, nil
}

// v1Targets is what a run knows about the shared rooms it filled.
type v1Targets struct {
	freshHomes map[string]bool     // home rooms this run created, so their readers start caught up like any imported room
	freshSubs  map[string]bool     // sub-rooms this run created
	groups     map[string][]string // target room -> the v1 rooms placed in it, in order
	labels     map[string]string
	order      []string
}

// memoryOff applies one rule to every shared target, the project root and a shared sub-room alike: if any v1 room placed in it had
// memory off, the target has memory off (the mirror of "open if any room is open"). It is recorded per v1 room, so a rerun does not
// repeat it, and each one is listed (MemoryOffTargets) and counted so the owner is told.
func (t *v1Targets) memoryOff(ctx context.Context, tx *sql.Tx, sourceKey string, room v1ImportRoom, target, label string, report *V1ImportReport) error {
	if room.MemoryEnabled == nil || *room.MemoryEnabled {
		return nil
	}
	if _, marked, err := importMapID(ctx, tx, sourceKey, "room_memory", room.ID); err != nil || marked {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE room SET memory_enabled=0 WHERE id=? AND memory_enabled IS NULL", target)
	if err != nil {
		return err
	}
	if err := addImportMap(ctx, tx, sourceKey, "room_memory", room.ID, target+"|"+room.ID); err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n > 0 {
		report.Imported["room_memory_settings"]++
	}
	if label = firstNonEmpty(label, t.labels[target]); label == "" {
		label = "a shared room"
	}
	report.MemoryOffTargets = append(report.MemoryOffTargets, fmt.Sprintf("memory is off for %s because v1 room %s had it off", label, room.ID))
	return nil
}

func newV1Targets() *v1Targets {
	return &v1Targets{freshHomes: map[string]bool{}, freshSubs: map[string]bool{}, groups: map[string][]string{}, labels: map[string]string{}}
}

// warnings lists the targets that more than one v1 room was placed in during this run.
func (t *v1Targets) warnings() []string {
	var out []string
	for _, id := range t.order {
		if rooms := t.groups[id]; len(rooms) > 1 {
			out = append(out, fmt.Sprintf("%d v1 rooms are merged into %s: %s", len(rooms), t.labels[id], strings.Join(rooms, ", ")))
		}
	}
	return out
}

// place puts one v1 room into the project's home room or a shared sub-room and records it. It returns the v2 room's id.
func (t *v1Targets) place(ctx context.Context, tx *sql.Tx, data *v1ImportData, mapping V1ImportMap, report *V1ImportReport, room v1ImportRoom, spec V1ImportRoom,
	projectID, projectName, ownerID string, userIDs map[string]string, newRooms map[string]bool) (string, error) {
	if placed, present, err := importMapID(ctx, tx, mapping.SourceKey, "room_in", room.ID); err != nil {
		return "", err
	} else if present {
		report.AlreadyPresent["rooms"]++
		report.AlreadyPresent["historical_members"] += len(room.Members)
		existing := strings.SplitN(placed, "|", 2)[0]
		if err := t.memoryOff(ctx, tx, mapping.SourceKey, room, existing, "", report); err != nil {
			return "", err
		}
		return existing, nil
	}
	home, err := v1HomeRoom(ctx, tx, mapping.SourceKey, spec.Project, projectID)
	if err != nil {
		return "", err
	}
	createdAt, _ := v1ImportTimestamp(room.CreatedAt)
	if projectName == "" {
		if err := tx.QueryRowContext(ctx, "SELECT name FROM project WHERE id=?", projectID).Scan(&projectName); err != nil {
			return "", err
		}
	}
	target, label := home, "⌂ "+projectName
	if v1RoomTarget(spec, room.ID) == V1TargetSub {
		title := v1SubRoomTitle(spec)
		label = title + " under ⌂ " + projectName
		key := v1SubRoomKey(spec.Project, title)
		sub, present, err := importMapID(ctx, tx, mapping.SourceKey, "room", key)
		if err != nil {
			return "", err
		}
		if !present && title == GeneralRoomTitle {
			// A project made in v2 already has its "general" task room beside the home room: the import fills that one instead of
			// making a second room of the same name.
			err := tx.QueryRowContext(ctx, "SELECT id FROM room WHERE project_id=? AND parent_id=? AND kind='task' AND title=? ORDER BY created_at,id LIMIT 1", projectID, home, title).Scan(&sub)
			switch {
			case err == nil:
				if err := addImportMap(ctx, tx, mapping.SourceKey, "room", key, sub); err != nil {
					return "", err
				}
				present = true
			case !errors.Is(err, sql.ErrNoRows):
				return "", err
			}
		}
		if present {
			var owner string
			if err := tx.QueryRowContext(ctx, "SELECT project_id FROM room WHERE id=?", sub).Scan(&owner); err != nil || owner != projectID {
				return "", fmt.Errorf("the shared sub-room %q recorded for project %q belongs to another project; its ledger row is wrong (%v)", title, spec.Project, err)
			}
		}
		if !present {
			if sub, err = newID("room"); err != nil {
				return "", err
			}
			lifecycle, closedAt := "closed", any(timestamp())
			if spec.Lifecycle == "open" {
				lifecycle, closedAt = "open", nil
			}
			createdBy := room.CreatedBy
			if mapped := userIDs[createdBy]; mapped != "" {
				createdBy = mapped
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO room(id,project_id,parent_id,depth,kind,lifecycle,title,created_by,created_at,closed_at) VALUES (?,?,?,1,'task',?,?,?,?,?)",
				sub, projectID, home, lifecycle, title, createdBy, createdAt, closedAt); err != nil {
				return "", err
			}
			if err := addImportMap(ctx, tx, mapping.SourceKey, "room", key, sub); err != nil {
				return "", err
			}
			t.freshSubs[sub] = true
			newRooms[sub] = true
		} else if t.freshSubs[sub] {
			newRooms[sub] = true
		}
		if present && t.freshSubs[sub] && spec.Lifecycle == "open" {
			// Rooms sharing a sub-room: it is open if any of them is asked to be.
			if _, err := tx.ExecContext(ctx, "UPDATE room SET lifecycle='open',closed_at=NULL WHERE id=?", sub); err != nil {
				return "", err
			}
		}
		target = sub
	} else {
		if t.freshHomes[home] {
			newRooms[home] = true
		}
	}
	if err := t.memoryOff(ctx, tx, mapping.SourceKey, room, target, label, report); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO legacy_room(room_id,data_json) VALUES (?,?)", target, data.RoomRaw[room.ID]); err != nil {
		return "", err
	}
	if err := addImportMap(ctx, tx, mapping.SourceKey, "room_in", room.ID, target+"|"+room.ID); err != nil {
		return "", err
	}
	if _, seen := t.groups[target]; !seen {
		t.order = append(t.order, target)
		t.labels[target] = label
	}
	t.groups[target] = append(t.groups[target], room.ID)
	report.Imported["rooms"]++
	for _, member := range room.Members {
		kind := v1ImportKind(member, data.Agents)
		name := member
		if agent, ok := data.Agents[member]; ok && agent.Name != "" {
			name = agent.Name
			if kind == "agent" {
				name = member
				if agent.Harness != "" || agent.Model != "" {
					name += " · " + strings.Trim(strings.Join([]string{agent.Harness, agent.Model}, "/"), "/")
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO legacy_room_member(room_id,legacy_id,kind,label) VALUES (?,?,?,?)", target, member, kind, name); err != nil {
			return "", err
		}
		report.Imported["historical_members"]++
		// The home room is readable by every project member already; a sub-room also lists the people who were in the v1 room.
		if target != home && kind == "user" && userIDs[member] != "" {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO room_member(room_id,member_kind,member_id,joined_at) VALUES (?,'user',?,?)", target, userIDs[member], createdAt); err != nil {
				return "", err
			}
		}
	}
	return target, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
