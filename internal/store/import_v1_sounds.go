package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-json"
)

// v1ImportSound is one entry of v1's sounds table: an owner-uploaded custom
// notification sound whose bytes live in <server>/sounds/<uuid>.<ext>.
type v1ImportSound struct {
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	UploadedAt string `json:"uploaded_at"`
	Ext        string `json:"ext"` // empty means a legacy mp3
	// Malformed marks a row whose JSON could not be read; it is skipped with a
	// reason at import time rather than guessed at.
	Malformed bool `json:"-"`
}

var v1SoundIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// loadV1Sounds reads the sounds table. A backup without one has no custom
// sounds (v1 creates the table when it first persists a sound).
func loadV1Sounds(ctx context.Context, db *sql.DB) ([]v1ImportSound, error) {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sounds'`).Scan(&present); err != nil || present == 0 {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id,data FROM sounds`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sounds []v1ImportSound
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var sound v1ImportSound
		if err := json.Unmarshal([]byte(raw), &sound); err != nil {
			// Not fatal: the importer skips it and says why.
			sound = v1ImportSound{UUID: id, Malformed: true}
		}
		if sound.UUID == "" {
			sound.UUID = id
		}
		sounds = append(sounds, sound)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Oldest upload first, then by id, so when the pool cap cuts the list off
	// the sounds kept are the ones that were there longest. A row with no
	// readable upload time sorts last.
	key := func(sound v1ImportSound) string {
		if stamp, err := v1ImportTimestamp(sound.UploadedAt); err == nil && stamp != "" {
			return stamp
		}
		return "~"
	}
	sort.SliceStable(sounds, func(i, j int) bool {
		if a, b := key(sounds[i]), key(sounds[j]); a != b {
			return a < b
		}
		return sounds[i].UUID < sounds[j].UUID
	})
	return sounds, nil
}

// v1SoundFile locates a sound's bytes in the same root the attachments use.
func v1SoundFile(sourceDir string, sound v1ImportSound, ext string) string {
	return filepath.Join(v1ImportServerRoot(sourceDir), "sounds", sound.UUID+"."+ext)
}

// importV1Sounds carries v1's uploaded notification sounds into the sound
// table and the sounds directory. Each sound is judged on its own: one that is
// missing, malformed, not a regular file, over the per-file cap, or that would
// exceed the file-count cap (counting sounds already in v2) is skipped and
// reported with its reason, and never fails the import. The checks are the
// upload path's own (SoundFormatProblem, SoundHeaderProblem, checkSoundCount,
// SoundDisplayName), so the two cannot drift apart. Bytes are
// read, checked, and written under a new ID in a staged file that the caller
// removes if the transaction does not commit, so nothing is half written; a
// dry run checks everything and writes nothing. The ledger makes a rerun skip
// sounds already imported, even ones the owner deleted since.
//
// v1's sound choice is one global server setting (notification_sound), while
// v2's is a per-browser preference, so the choice itself is not imported (it
// is already listed among the skipped settings).
func importV1Sounds(ctx context.Context, tx *sql.Tx, sourceDir, soundDir, ownerID string, data *v1ImportData, mapping V1ImportMap, dryRun bool, staged *[]string, report *V1ImportReport) error {
	if len(data.Sounds) == 0 {
		return nil
	}
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sound`).Scan(&count); err != nil {
		return err
	}
	skip := func(sound v1ImportSound, reason string) {
		report.Skipped["sounds"]++
		name := sound.Name
		if name == "" {
			name = sound.UUID
		}
		report.SkippedSounds[sound.UUID] = fmt.Sprintf("%s: %s", name, reason)
	}
	for index, sound := range data.Sounds {
		if !v1SoundIDPattern.MatchString(sound.UUID) {
			// The id cannot name a file or a ledger entry, so it cannot key the
			// report either; number the entry instead.
			report.Skipped["sounds"]++
			report.SkippedSounds[fmt.Sprintf("(unusable id #%d)", index+1)] = "a sound has an id that cannot name a file"
			continue
		}
		if sound.Malformed {
			skip(sound, "the sound's database entry could not be read")
			continue
		}
		if _, found, err := importMapID(ctx, tx, mapping.SourceKey, "sound", sound.UUID); err != nil {
			return err
		} else if found {
			report.AlreadyPresent["sounds"]++
			continue
		}
		ext := strings.ToLower(sound.Ext)
		if ext == "" {
			ext = "mp3"
		}
		// The extension is untrusted until it passes the upload check (the size
		// is checked for real once the file has been found).
		if reason := SoundFormatProblem(ext, 1); reason != "" {
			skip(sound, reason)
			continue
		}
		path := v1SoundFile(sourceDir, sound, ext)
		info, err := os.Lstat(path)
		if err != nil {
			skip(sound, "the sound file is missing from the backup")
			continue
		}
		if !info.Mode().IsRegular() {
			skip(sound, "the sound file is not a regular file")
			continue
		}
		if reason := SoundFormatProblem(ext, info.Size()); reason != "" {
			skip(sound, reason)
			continue
		}
		bytes, err := os.ReadFile(path)
		if err != nil {
			skip(sound, "the sound file could not be read")
			continue
		}
		// The file may have grown between the stat and the read.
		if reason := SoundFormatProblem(ext, int64(len(bytes))); reason != "" {
			skip(sound, reason)
			continue
		}
		if reason := SoundHeaderProblem(ext, bytes); reason != "" {
			skip(sound, reason)
			continue
		}
		if err := checkSoundCount(count); err != nil {
			skip(sound, err.Error())
			continue
		}
		id, err := newID("snd")
		if err != nil {
			return err
		}
		createdAt, err := v1ImportTimestamp(sound.UploadedAt)
		if err != nil {
			createdAt = timestamp()
		}
		if !dryRun {
			if err := os.MkdirAll(soundDir, 0o700); err != nil {
				return err
			}
			destination := filepath.Join(soundDir, id+"."+ext)
			if err := os.WriteFile(destination, bytes, 0o600); err != nil {
				_ = os.Remove(destination)
				return err
			}
			*staged = append(*staged, destination)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sound(id,name,ext,size,created_by,created_at) VALUES (?,?,?,?,?,?)`,
			id, SoundDisplayName(sound.Name, ext), ext, int64(len(bytes)), ownerID, createdAt); err != nil {
			return err
		}
		if err := addImportMap(ctx, tx, mapping.SourceKey, "sound", sound.UUID, id); err != nil {
			return err
		}
		count++
		report.Imported["sounds"]++
	}
	return nil
}
