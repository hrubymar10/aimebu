package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
)

// ErrRoomNotRemovable: a home room, a private chat, or a room with a live worktree.
var ErrRoomNotRemovable = errors.New("room_not_removable")

// ErrRoomNotClosed: a room is removed only after it has been archived (closed).
var ErrRoomNotClosed = errors.New("room_not_closed")

// RemoveRoom deletes an archived task room and everything that belongs to it: its
// messages, reactions, notes, turns, inbox items, usage records, schedules, task notes,
// members, read cursors and the attachment rows and files. Rows elsewhere that merely
// point at the room or one of its messages (events, memory records, an agent's home
// room) keep existing with the reference cleared, so project history and curated
// knowledge are not lost. The caller has already checked who may do this.
func (s *Store) RemoveRoom(ctx context.Context, id string) error {
	var files []string
	err := s.Tx(ctx, func(tx *Tx) error {
		var kind, lifecycle, projectID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT kind,lifecycle,project_id FROM room WHERE id=?`, id).Scan(&kind, &lifecycle, &projectID); err != nil {
			return err
		}
		switch {
		case kind == "home":
			return ErrHomeRoom
		case kind == "dm":
			return ErrRoomNotRemovable
		case lifecycle != "closed":
			return ErrRoomNotClosed
		}
		var children int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room WHERE parent_id=?`, id).Scan(&children); err != nil {
			return err
		}
		if children > 0 {
			return ErrOpenChildren
		}
		var worktree int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_worktree WHERE room_id=? AND state<>'removed'`, id).Scan(&worktree); err != nil {
			return err
		}
		if worktree > 0 {
			return ErrRoomNotRemovable
		}
		// A second writer's worktree is on disk and may hold work: removing the room would delete its record (and so its place in Health) and
		// leave the folder behind. It has to be removed first.
		var agentTrees int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent_worktree WHERE room_id=? AND state<>'removed'`, id).Scan(&agentTrees); err != nil {
			return err
		}
		if agentTrees > 0 {
			return ErrRoomNotRemovable
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT path FROM attachment WHERE room_id=? OR message_id IN (SELECT id FROM message WHERE room_id=?)`, id, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return err
			}
			files = append(files, path)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM attachment WHERE room_id=? OR message_id IN (SELECT id FROM message WHERE room_id=?)`, id, id); err != nil {
			return err
		}
		graph, err := tx.foreignKeyGraph()
		if err != nil {
			return err
		}
		if err := tx.deleteCascade(graph, "room", "id=?", []any{id}, 0); err != nil {
			return err
		}
		// The room is gone, so the event carries its id in the payload only.
		_, err = tx.AppendEvent(Event{Kind: "room_removed", ProjectID: projectID, PayloadJSON: payloadJSON(map[string]string{"room_id": id})})
		return err
	})
	if err != nil {
		return err
	}
	for _, path := range files {
		var shared int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachment WHERE path=?`, path).Scan(&shared); err == nil && shared > 0 {
			continue
		}
		// The room is already gone; a file that cannot be removed is a leftover to log, not a failed removal.
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			log.Printf("remove room %s: attachment file %s left behind: %v", id, path, removeErr)
		}
	}
	return nil
}

type foreignKey struct {
	child, childColumn, parent, parentColumn string
	notNull                                  bool
}

// foreignKeyGraph lists every foreign key in the schema, with whether the child column is required.
func (tx *Tx) foreignKeyGraph() ([]foreignKey, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	primary := map[string]string{}
	required := map[string]bool{}
	for _, table := range tables {
		info, err := tx.tx.QueryContext(tx.ctx, fmt.Sprintf(`PRAGMA table_info(%q)`, table))
		if err != nil {
			return nil, err
		}
		for info.Next() {
			var cid, notNull, pk int
			var name, ctype string
			var dflt sql.NullString
			if err := info.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
				info.Close()
				return nil, err
			}
			required[table+"."+name] = notNull == 1 || pk > 0
			if pk == 1 {
				primary[table] = name
			}
		}
		err = info.Err()
		info.Close()
		if err != nil {
			return nil, err
		}
	}
	var graph []foreignKey
	for _, table := range tables {
		fks, err := tx.tx.QueryContext(tx.ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%q)`, table))
		if err != nil {
			return nil, err
		}
		for fks.Next() {
			var id, seq int
			var parent, from, onUpdate, onDelete, match string
			var to sql.NullString
			if err := fks.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				fks.Close()
				return nil, err
			}
			column := to.String
			if column == "" {
				column = primary[parent]
			}
			graph = append(graph, foreignKey{child: table, childColumn: from, parent: parent, parentColumn: column, notNull: required[table+"."+from]})
		}
		err = fks.Err()
		fks.Close()
		if err != nil {
			return nil, err
		}
	}
	return graph, nil
}

// deleteCascade deletes the rows of table matching cond and, first, whatever depends on them:
// rows whose required foreign key points at them are deleted the same way, rows whose key is
// optional only lose the reference. Self references are within the deleted set by construction.
func (tx *Tx) deleteCascade(graph []foreignKey, table, cond string, args []any, depth int) error {
	if depth > 8 {
		return fmt.Errorf("room removal: foreign keys nest too deeply at %s", table)
	}
	for _, fk := range graph {
		if fk.parent != table || fk.child == table {
			continue
		}
		childCond := fmt.Sprintf("%s IN (SELECT %s FROM %s WHERE %s)", fk.childColumn, fk.parentColumn, table, cond)
		if fk.notNull {
			if err := tx.deleteCascade(graph, fk.child, childCond, args, depth+1); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET %s=NULL WHERE %s`, fk.child, fk.childColumn, childCond), args...); err != nil {
			return err
		}
	}
	_, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE %s`, table, cond), args...)
	return err
}
