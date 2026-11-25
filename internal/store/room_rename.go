package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRoomTitleRunes is the longest room title a rename accepts.
const MaxRoomTitleRunes = 80

// ErrRoomNotRenamable is a rename of a private chat, which has no title of its own (the two people's names stand for it).
var ErrRoomNotRenamable = errors.New("room cannot be renamed")

// ErrInvalidRoomTitle is a title that is empty, too long, or has a control character such as a line break in it.
var ErrInvalidRoomTitle = errors.New("invalid room title")

// NormalizeRoomTitle trims a title and checks it: not empty, at most MaxRoomTitleRunes characters, and no control character or line or
// paragraph separator (a line break or a tab would break the room list and the header).
func NormalizeRoomTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" || utf8.RuneCountInString(title) > MaxRoomTitleRunes || !utf8.ValidString(title) {
		return "", ErrInvalidRoomTitle
	}
	for _, r := range title {
		// Control characters, and the two Unicode separators that start a new line without being control characters (U+2028, U+2029).
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return "", ErrInvalidRoomTitle
		}
	}
	return title, nil
}

// RenameRoom changes a room's title and records room_renamed. A home room is named after its project, so renaming it renames the project
// (the same path as the home room's project settings, which also keeps the home room's title in step) and the home room alike; there is one name, not a
// room title that can drift from it. A private chat cannot be renamed. The title must already be normalized (NormalizeRoomTitle). A task
// room that already has the title changes nothing and records nothing; a home room is unchanged only when its project has the name too,
// so a title that drifted from the project's name is repaired by the rename.
func (s *Store) RenameRoom(ctx context.Context, roomID, title, actorID string) (Room, error) {
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, kind, previous, projectName string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.project_id,r.kind,r.title,p.name FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`, roomID).Scan(&projectID, &kind, &previous, &projectName); err != nil {
			return err
		}
		if kind == "dm" {
			return ErrRoomNotRenamable
		}
		// Nothing to do when the room already has the title; for the home room, when the project has the name as well. A home room whose
		// title drifted from the project's name is not unchanged by being given the project's name, nor by being given its own title.
		if title == previous && (kind != "home" || title == projectName) {
			return nil
		}
		if kind == "home" {
			if _, err := tx.updateProject(projectID, &title, nil, actorID, true); err != nil {
				return err
			}
		} else if _, err := tx.Exec(`UPDATE room SET title=? WHERE id=?`, title, roomID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "room_renamed", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"room_id": roomID, "title": title, "previous_title": previous, "actor_id": actorID})})
		return err
	})
	if err != nil {
		return Room{}, err
	}
	room, err := s.RoomByID(ctx, roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return Room{}, err
	}
	return room, err
}
