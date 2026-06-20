package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/goccy/go-json"
)

// ── User sounds ────────────────────────────────────────────────────

const (
	maxSoundFiles     = 10
	maxSoundTotalSize = 20 * 1024 * 1024 // 20 MB
	maxSoundFileSize  = 1 * 1024 * 1024  // 1 MB per file
)

func (s *store) soundsDir() string {
	return filepath.Join(s.dir, "sounds")
}

func (s *store) soundsIndexPath() string {
	return filepath.Join(s.soundsDir(), "sounds.json")
}

func (s *store) loadSounds() {
	if s.db != nil {
		var sounds []SoundEntry
		if err := s.loadJSONRows("sounds", "id", func(_ string, data []byte) error {
			var entry SoundEntry
			if err := json.Unmarshal(data, &entry); err != nil {
				return err
			}
			if entry.UUID != "" {
				sounds = append(sounds, entry)
			}
			return nil
		}); err == nil {
			s.soundsMu.Lock()
			s.sounds = sounds
			s.soundsMu.Unlock()
			return
		}
	}
	data, err := os.ReadFile(s.soundsIndexPath())
	if err != nil {
		return
	}
	var env struct {
		Sounds []SoundEntry `json:"sounds"`
	}
	if json.Unmarshal(data, &env) == nil {
		s.soundsMu.Lock()
		s.sounds = env.Sounds
		s.soundsMu.Unlock()
	}
}

// persistSoundsLocked writes the sounds index to disk. Must be called with
// soundsMu held (read or write). Does not acquire the lock.
func (s *store) persistSoundsLocked() {
	if s.db != nil {
		rows := make(map[string]any, len(s.sounds))
		for _, entry := range s.sounds {
			rows[entry.UUID] = entry
		}
		if err := s.replaceJSONRows("sounds", "id", rows); err != nil {
			log.Printf("aimebu: persist sounds sqlite: %v", err)
		}
		return
	}
	env := struct {
		Sounds []SoundEntry `json:"sounds"`
	}{Sounds: s.sounds}
	data, err := json.MarshalIndent(env, "", "  ")
	if err == nil {
		atomicWrite(s.soundsIndexPath(), data)
	}
}

func (s *store) listSounds() []SoundEntry {
	s.soundsMu.RLock()
	defer s.soundsMu.RUnlock()
	out := make([]SoundEntry, len(s.sounds))
	copy(out, s.sounds)
	return out
}

// addSound stores an uploaded MP3 file and adds it to the index.
// data must have already been validated (size, header).
func (s *store) addSound(displayName string, data []byte, ext string) (SoundEntry, error) {
	// Cap and total-size checks under write lock before touching the filesystem.
	s.soundsMu.Lock()
	if len(s.sounds) >= maxSoundFiles {
		s.soundsMu.Unlock()
		return SoundEntry{}, fmt.Errorf("sound limit reached (%d files); delete one first", maxSoundFiles)
	}
	var total int64
	for _, e := range s.sounds {
		total += e.Size
	}
	if total+int64(len(data)) > maxSoundTotalSize {
		s.soundsMu.Unlock()
		return SoundEntry{}, fmt.Errorf("total sound storage limit reached (%.0f MB)", float64(maxSoundTotalSize)/1024/1024)
	}
	s.soundsMu.Unlock()

	// Write to disk outside the lock (no lock needed — UUID is unique).
	uuid := randomUUID()
	if err := os.MkdirAll(s.soundsDir(), 0o755); err != nil {
		return SoundEntry{}, fmt.Errorf("create sounds dir: %w", err)
	}
	path := filepath.Join(s.soundsDir(), uuid+"."+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return SoundEntry{}, fmt.Errorf("write sound file: %w", err)
	}

	entry := SoundEntry{
		UUID:       uuid,
		Name:       displayName,
		Size:       int64(len(data)),
		UploadedAt: now(),
		Ext:        ext,
	}

	// Re-acquire write lock to append + persist. Re-check caps in case a concurrent
	// upload raced past the first check between the unlock and this re-lock.
	s.soundsMu.Lock()
	if len(s.sounds) >= maxSoundFiles {
		s.soundsMu.Unlock()
		_ = os.Remove(path)
		return SoundEntry{}, fmt.Errorf("sound limit reached (%d files); delete one first", maxSoundFiles)
	}
	var total2 int64
	for _, e := range s.sounds {
		total2 += e.Size
	}
	if total2+int64(len(data)) > maxSoundTotalSize {
		s.soundsMu.Unlock()
		_ = os.Remove(path)
		return SoundEntry{}, fmt.Errorf("total sound storage limit reached (%.0f MB)", float64(maxSoundTotalSize)/1024/1024)
	}
	s.sounds = append(s.sounds, entry)
	s.persistSoundsLocked()
	s.soundsMu.Unlock()

	return entry, nil
}

// deleteSound removes a user sound by UUID. Returns false if the UUID is unknown.
func (s *store) deleteSound(uuid string) bool {
	s.soundsMu.Lock()
	defer s.soundsMu.Unlock()

	for i, e := range s.sounds {
		if e.UUID != uuid {
			continue
		}
		s.sounds = append(s.sounds[:i], s.sounds[i+1:]...)
		fileExt := e.Ext
		if fileExt == "" {
			fileExt = "mp3"
		}
		_ = os.Remove(filepath.Join(s.soundsDir(), uuid+"."+fileExt))
		s.persistSoundsLocked()
		return true
	}
	return false
}

// soundFilePath returns the on-disk path and extension ("mp3" or "wav") for a
// user sound UUID, or ("", "") if unknown. Legacy entries with empty Ext are
// treated as "mp3".
func (s *store) soundFilePath(uuid string) (string, string) {
	s.soundsMu.RLock()
	defer s.soundsMu.RUnlock()
	for _, e := range s.sounds {
		if e.UUID == uuid {
			ext := e.Ext
			if ext == "" {
				ext = "mp3"
			}
			return filepath.Join(s.soundsDir(), uuid+"."+ext), ext
		}
	}
	return "", ""
}
