package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Cleanup settings live in global_setting; every value has a default so a fresh install needs no row.
const (
	cleanupEnabledKey    = "cleanup.enabled"
	cleanupHourKey       = "cleanup.hour"
	cleanupOrphanDaysKey = "cleanup.orphan_days"
	cleanupLastRunKey    = "cleanup.last_run"
	cleanupAttachmentKey = "cleanup.attachment_days"
)

type CleanupSettings struct {
	Enabled    bool `json:"enabled"`
	Hour       int  `json:"hour"`
	OrphanDays int  `json:"orphan_days"`
	// AttachmentDays is how old an attachment must be before its file is removed (the message stays); 0 keeps them for ever.
	AttachmentDays int    `json:"attachment_days"`
	LastRun        string `json:"last_run,omitempty"`
}

// MaxAttachmentDays bounds the attachment retention setting (ten years).
const MaxAttachmentDays = 3650

func (s *Store) globalSettingValue(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM global_setting WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

func (s *Store) setGlobalSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO global_setting(key,value,updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, value, timestamp())
	return err
}

// CleanupSettings returns the scheduled cleanup settings: on by default, at 03:00 server time, with worktrees counted as orphans
// after 14 idle days.
func (s *Store) CleanupSettings(ctx context.Context) (CleanupSettings, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return CleanupSettings{}, err
	}
	out := CleanupSettings{Enabled: true, Hour: 3, OrphanDays: 14, AttachmentDays: 90}
	if value, ok, err := s.globalSettingValue(ctx, cleanupEnabledKey); err != nil {
		return out, err
	} else if ok {
		out.Enabled = value == "1"
	}
	if value, ok, err := s.globalSettingValue(ctx, cleanupHourKey); err != nil {
		return out, err
	} else if n, convErr := strconv.Atoi(value); ok && convErr == nil && n >= 0 && n <= 23 {
		out.Hour = n
	}
	if value, ok, err := s.globalSettingValue(ctx, cleanupOrphanDaysKey); err != nil {
		return out, err
	} else if n, convErr := strconv.Atoi(value); ok && convErr == nil && n >= 1 && n <= 365 {
		out.OrphanDays = n
	}
	if value, ok, err := s.globalSettingValue(ctx, cleanupAttachmentKey); err != nil {
		return out, err
	} else if n, convErr := strconv.Atoi(value); ok && convErr == nil && n >= 0 && n <= MaxAttachmentDays {
		out.AttachmentDays = n
	}
	last, _, err := s.globalSettingValue(ctx, cleanupLastRunKey)
	out.LastRun = last
	return out, err
}

// SetCleanupSettings stores the fields that are not nil; a value outside its range is ErrInvalidTransition.
func (s *Store) SetCleanupSettings(ctx context.Context, enabled *bool, hour, orphanDays, attachmentDays *int) (CleanupSettings, error) {
	if (hour != nil && (*hour < 0 || *hour > 23)) || (orphanDays != nil && (*orphanDays < 1 || *orphanDays > 365)) ||
		(attachmentDays != nil && (*attachmentDays < 0 || *attachmentDays > MaxAttachmentDays)) {
		return CleanupSettings{}, ErrInvalidTransition
	}
	if enabled != nil {
		value := "0"
		if *enabled {
			value = "1"
		}
		if err := s.setGlobalSetting(ctx, cleanupEnabledKey, value); err != nil {
			return CleanupSettings{}, err
		}
	}
	if hour != nil {
		if err := s.setGlobalSetting(ctx, cleanupHourKey, strconv.Itoa(*hour)); err != nil {
			return CleanupSettings{}, err
		}
	}
	if orphanDays != nil {
		if err := s.setGlobalSetting(ctx, cleanupOrphanDaysKey, strconv.Itoa(*orphanDays)); err != nil {
			return CleanupSettings{}, err
		}
	}
	if attachmentDays != nil {
		if err := s.setGlobalSetting(ctx, cleanupAttachmentKey, strconv.Itoa(*attachmentDays)); err != nil {
			return CleanupSettings{}, err
		}
	}
	return s.CleanupSettings(ctx)
}

// MarkCleanupRun records the day of the scheduled run (YYYY-MM-DD, server time) so it happens once a day.
func (s *Store) MarkCleanupRun(ctx context.Context, day string) error {
	return s.setGlobalSetting(ctx, cleanupLastRunKey, day)
}

type CleanupEntry struct {
	ID        string `json:"id"`
	Category  string `json:"category"`
	Target    string `json:"target"`
	ProjectID string `json:"project_id,omitempty"`
	Reason    string `json:"reason"`
	Bytes     int64  `json:"bytes"`
	Action    string `json:"action"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
}

// AppendCleanupLog records what a cleanup did (or, for a dry run, would do).
func (s *Store) AppendCleanupLog(ctx context.Context, entries []CleanupEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.Tx(ctx, func(tx *Tx) error {
		var seq int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(MAX(seq),0) FROM cleanup_log`).Scan(&seq); err != nil {
			return err
		}
		for _, entry := range entries {
			id, err := newID("clean")
			if err != nil {
				return err
			}
			seq++
			var project any
			if entry.ProjectID != "" {
				project = entry.ProjectID
			}
			if _, err := tx.Exec(`INSERT INTO cleanup_log(id,seq,category,target,project_id,reason,bytes,action,actor,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
				id, seq, entry.Category, entry.Target, project, entry.Reason, entry.Bytes, entry.Action, entry.Actor, timestamp()); err != nil {
				return err
			}
		}
		return nil
	})
}

// CleanupLog returns the newest entries first.
func (s *Store) CleanupLog(ctx context.Context, limit int) ([]CleanupEntry, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,category,target,COALESCE(project_id,''),reason,bytes,action,actor,created_at FROM cleanup_log ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CleanupEntry{}
	for rows.Next() {
		var entry CleanupEntry
		if err := rows.Scan(&entry.ID, &entry.Category, &entry.Target, &entry.ProjectID, &entry.Reason, &entry.Bytes, &entry.Action, &entry.Actor, &entry.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// TrimCleanupLog drops cleanup log rows older than the cutoff and returns how many.
func (s *Store) TrimCleanupLog(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM cleanup_log WHERE created_at<?`, before.UTC().Format(timestampLayout))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DBPath is the database file; the server directory and the config root are its parents.
func (s *Store) DBPath() string { return s.dbPath }

// AttachmentDir is where uploaded blobs live.
func (s *Store) AttachmentDir() string { return s.attachmentDir }

// AttachmentBytesByProject sums the registered attachment sizes per project.
func (s *Store) AttachmentBytesByProject(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(project_id,''), COALESCE(SUM(size),0) FROM attachment WHERE removed_at IS NULL GROUP BY project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var project string
		var bytes int64
		if err := rows.Scan(&project, &bytes); err != nil {
			return nil, err
		}
		out[project] = bytes
	}
	return out, rows.Err()
}

// RegisteredAttachmentPaths is the set of blob paths the registry knows, to find files nothing points at.
func (s *Store) RegisteredAttachmentPaths(ctx context.Context) (map[string]bool, error) {
	// A removed attachment's file is gone; if it somehow is not, it is an orphan like any other and the sweep collects it.
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM attachment WHERE removed_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out[filepath.Clean(s.resolveAttachmentPath(path))] = true
	}
	return out, rows.Err()
}

// DatabaseFootprint is the size on disk of the database and its write-ahead log, and the share of its pages that are free.
type DatabaseFootprint struct {
	Bytes      int64   `json:"bytes"`
	FreeRatio  float64 `json:"free_ratio"`
	FreeBytes  int64   `json:"free_bytes"`
	PageBytes  int64   `json:"-"`
	TotalPages int64   `json:"-"`
}

func (s *Store) DatabaseFootprint(ctx context.Context) (DatabaseFootprint, error) {
	var out DatabaseFootprint
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(s.dbPath + suffix); err == nil {
			out.Bytes += info.Size()
		}
	}
	var pageSize, pages, free int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return out, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return out, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return out, err
	}
	out.PageBytes, out.TotalPages = pageSize, pages
	out.FreeBytes = free * pageSize
	if pages > 0 {
		out.FreeRatio = float64(free) / float64(pages)
	}
	return out, nil
}

// Compact folds the write-ahead log back into the database and rebuilds it without its free pages.
func (s *Store) Compact(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// RoomWorktreeInstance is one ready worktree on one machine, with what the cleanup needs to judge it.
type RoomWorktreeInstance struct {
	RoomID      string
	RoomTitle   string
	Lifecycle   string
	ProjectID   string
	ProjectName string
	MachineID   string
	Path        string
	BaseRef     string
	BaseCommit  string
	DirtyBase   int
	InUse       bool
	// AgentSlug is set for a worktree that belongs to one agent (a second writer's) rather than to the room; Name is the directory under
	// .aimebu/worktrees either way (the room's ID, or <room-id>-<slug>).
	AgentSlug string
	AgentID   string
	Name      string
}

// ReadyWorktrees lists every worktree that exists, for rooms open or closed: each room's own, and the ones second writers work in.
func (s *Store) ReadyWorktrees(ctx context.Context) ([]RoomWorktreeInstance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.room_id,r.title,r.lifecycle,r.project_id,p.name,i.machine_id,i.canonical_path,w.base_ref,i.base_commit,i.dirty_base_count,
		EXISTS(SELECT 1 FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.machine_id=i.machine_id AND c.canonical_path=i.canonical_path
			AND a.revoked_at IS NULL AND (a.desired!='stopped' OR a.observed!='stopped')),'',w.room_id,''
		FROM room_worktree w JOIN room r ON r.id=w.room_id JOIN project p ON p.id=r.project_id
		JOIN room_worktree_instance i ON i.room_id=w.room_id
		WHERE i.state IN ('ready','preserved') AND i.canonical_path!=''
		UNION ALL
		SELECT g.room_id,r.title,r.lifecycle,r.project_id,p.name,g.machine_id,g.canonical_path,g.base_ref,g.base_commit,g.dirty_base_count,
		EXISTS(SELECT 1 FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.machine_id=g.machine_id AND c.canonical_path=g.canonical_path
			AND a.revoked_at IS NULL AND (a.desired!='stopped' OR a.observed!='stopped')),ag.slug,g.name,ag.id
		FROM agent_worktree g JOIN agent ag ON ag.id=g.agent_id JOIN room r ON r.id=g.room_id JOIN project p ON p.id=r.project_id
		WHERE g.state IN ('ready','preserved') AND g.canonical_path!=''
		ORDER BY 5,1,6,12`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoomWorktreeInstance
	for rows.Next() {
		var item RoomWorktreeInstance
		if err := rows.Scan(&item.RoomID, &item.RoomTitle, &item.Lifecycle, &item.ProjectID, &item.ProjectName, &item.MachineID, &item.Path, &item.BaseRef, &item.BaseCommit, &item.DirtyBase, &item.InUse, &item.AgentSlug, &item.Name, &item.AgentID); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ProjectNames maps project IDs to their names.
func (s *Store) ProjectNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name FROM project WHERE system=0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// OldAttachment is a message attachment past its retention.
type OldAttachment struct {
	ID        string
	Name      string
	Path      string
	ProjectID string
	Size      int64
}

// AttachmentsOlderThan lists the attachments of messages that arrived before cutoff and still have their file. An attachment arrives when
// it is uploaded, or, for one imported from v1 or here before the retention existed, when it was imported or the retention came in: it
// is never judged by the age it had before it came to v2.
func (s *Store) AttachmentsOlderThan(ctx context.Context, cutoff time.Time) ([]OldAttachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,filename,path,project_id,size FROM attachment
		WHERE removed_at IS NULL AND message_id IS NOT NULL
		AND (CASE WHEN arrived_at<>'' AND arrived_at>created_at THEN arrived_at ELSE created_at END)<? ORDER BY created_at,id`, cutoff.UTC().Format(timestampLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OldAttachment
	for rows.Next() {
		var item OldAttachment
		if err := rows.Scan(&item.ID, &item.Name, &item.Path, &item.ProjectID, &item.Size); err != nil {
			return nil, err
		}
		item.Path = s.resolveAttachmentPath(item.Path)
		out = append(out, item)
	}
	return out, rows.Err()
}

// MarkAttachmentRemoved records that retention removed an attachment's file, after days days, and says whether the file itself can
// go: false when another live record still points at the same path. The row and its message stay; only the marker is written.
func (s *Store) MarkAttachmentRemoved(ctx context.Context, id string, days int) (deleteFile bool, err error) {
	err = s.Tx(ctx, func(tx *Tx) error {
		var path string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT path FROM attachment WHERE id=? AND removed_at IS NULL`, id).Scan(&path); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE attachment SET removed_at=?,removed_after_days=? WHERE id=?`, timestamp(), days, id); err != nil {
			return err
		}
		var shared int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM attachment WHERE path=? AND removed_at IS NULL`, path).Scan(&shared); err != nil {
			return err
		}
		deleteFile = shared == 0
		return nil
	})
	return deleteFile, err
}
