package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Custom notification sounds. The owner uploads a small pool of MP3/WAV files
// that every signed-in user may choose from; the choice itself is a browser
// preference. Ten files of at most 1 MB bound the pool at 10 MB, so there is
// no separate total cap.
const (
	MaxSoundFiles    = 10
	MaxSoundFileSize = 1 << 20
	soundNameMax     = 64
)

var (
	ErrInvalidSound = errors.New("invalid sound")
	ErrSoundLimit   = errors.New("sound limit reached")
)

type Sound struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Ext        string `json:"ext"`
	Size       int64  `json:"size"`
	UploadedAt string `json:"uploaded_at"`
}

func (s *Store) soundDir() string { return filepath.Join(filepath.Dir(s.attachmentDir), "sounds") }

// ValidSoundHeader reports whether data starts like the declared container: an
// ID3v2 tag or MPEG sync word for mp3, a RIFF/WAVE header for wav.
func ValidSoundHeader(ext string, data []byte) bool {
	switch ext {
	case "mp3":
		return len(data) >= 4 && (data[0] == 'I' && data[1] == 'D' && data[2] == '3' || data[0] == 0xFF && data[1] >= 0xE0)
	case "wav":
		return len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WAVE"
	}
	return false
}

// SoundFormatProblem explains why a sound of this extension and size can never
// be stored, or returns "". Uploads and the v1 import both use it, so the
// limits live in one place; the header is checked separately once the bytes
// are read.
func SoundFormatProblem(ext string, size int64) string {
	switch {
	case ext != "mp3" && ext != "wav":
		return "only .mp3 and .wav sounds are supported"
	case size <= 0 || size > MaxSoundFileSize:
		return fmt.Sprintf("the file must be between 1 byte and %d MB", MaxSoundFileSize>>20)
	}
	return ""
}

// SoundHeaderProblem is the header half of SoundFormatProblem.
func SoundHeaderProblem(ext string, data []byte) string {
	if !ValidSoundHeader(ext, data) {
		return "the file is not a valid ." + ext + " file"
	}
	return ""
}

// checkSoundCount applies the pool size cap to the number of sounds already
// stored.
func checkSoundCount(count int64) error {
	if count >= MaxSoundFiles {
		return fmt.Errorf("%w: at most %d custom sounds; delete one first", ErrSoundLimit, MaxSoundFiles)
	}
	return nil
}

// SoundDisplayName applies the attachment name rules (base name only, control
// characters stripped) and then the shorter sound limit by rune count.
func SoundDisplayName(filename, ext string) string {
	name := safeAttachmentName(filename)
	if name == "attachment" {
		return "custom." + ext
	}
	if utf8.RuneCountInString(name) > soundNameMax {
		name = string([]rune(name)[:soundNameMax])
	}
	return name
}

func (s *Store) ListSounds(ctx context.Context) ([]Sound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,ext,size,created_at FROM sound ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sounds := []Sound{}
	for rows.Next() {
		var sound Sound
		if err := rows.Scan(&sound.ID, &sound.Name, &sound.Ext, &sound.Size, &sound.UploadedAt); err != nil {
			return nil, err
		}
		sounds = append(sounds, sound)
	}
	return sounds, rows.Err()
}

// SaveSound validates and stores one uploaded sound. The count cap is checked in
// the same transaction that inserts the row, so concurrent uploads cannot
// exceed them; the file is written first and removed again if the insert fails.
func (s *Store) SaveSound(ctx context.Context, userID, filename string, data []byte) (Sound, error) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
	if SoundFormatProblem(ext, int64(len(data))) != "" || SoundHeaderProblem(ext, data) != "" {
		return Sound{}, ErrInvalidSound
	}
	id, err := newID("snd")
	if err != nil {
		return Sound{}, err
	}
	if err := os.MkdirAll(s.soundDir(), 0o700); err != nil {
		return Sound{}, err
	}
	path := filepath.Join(s.soundDir(), id+"."+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Sound{}, err
	}
	sound := Sound{ID: id, Name: SoundDisplayName(filename, ext), Ext: ext, Size: int64(len(data)), UploadedAt: timestamp()}
	err = s.Tx(ctx, func(tx *Tx) error {
		var count int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM sound`).Scan(&count); err != nil {
			return err
		}
		if err := checkSoundCount(count); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sound(id,name,ext,size,created_by,created_at) VALUES(?,?,?,?,?,?)`, sound.ID, sound.Name, sound.Ext, sound.Size, userID, sound.UploadedAt)
		return err
	})
	if err != nil {
		_ = os.Remove(path)
		return Sound{}, err
	}
	return sound, nil
}

// SoundFile returns a sound's metadata and on-disk path.
func (s *Store) SoundFile(ctx context.Context, id string) (Sound, string, error) {
	var sound Sound
	err := s.db.QueryRowContext(ctx, `SELECT id,name,ext,size,created_at FROM sound WHERE id=?`, id).Scan(&sound.ID, &sound.Name, &sound.Ext, &sound.Size, &sound.UploadedAt)
	if err != nil {
		return Sound{}, "", err
	}
	return sound, filepath.Join(s.soundDir(), sound.ID+"."+sound.Ext), nil
}

// DeleteSound removes the row, then the file; a missing file is not an error.
func (s *Store) DeleteSound(ctx context.Context, id string) error {
	sound, path, err := s.SoundFile(ctx, id)
	if err != nil {
		return err
	}
	err = s.Tx(ctx, func(tx *Tx) error {
		result, err := tx.Exec(`DELETE FROM sound WHERE id=?`, sound.ID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
