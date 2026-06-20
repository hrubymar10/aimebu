package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

// ── Image attachments ──────────────────────────────────────────────

const (
	maxAttachmentFileSize    = 5 * 1024 * 1024
	maxMessageAttachments    = 4
	attachmentOrphanGrace    = time.Hour
	attachmentRegistryFile   = "attachments.json"
	attachmentDefaultFileExt = "bin"
)

func (s *store) attachmentsDir() string {
	return filepath.Join(s.dir, "attachments")
}

func (s *store) attachmentsIndexPath() string {
	return filepath.Join(s.attachmentsDir(), attachmentRegistryFile)
}

func attachmentExt(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return attachmentDefaultFileExt
	}
}

func attachmentMeta(entry AttachmentEntry) types.Attachment {
	return types.Attachment{
		ID:     entry.ID,
		Mime:   entry.Mime,
		Name:   entry.Name,
		Size:   entry.Size,
		Width:  entry.Width,
		Height: entry.Height,
	}
}

func (s *store) loadAttachments() {
	if s.db != nil {
		attachments := make(map[string]AttachmentEntry)
		if err := s.loadJSONRows("attachments", "id", func(_ string, data []byte) error {
			var entry AttachmentEntry
			if err := json.Unmarshal(data, &entry); err != nil {
				return err
			}
			if entry.ID == "" {
				return nil
			}
			if entry.Ext == "" {
				entry.Ext = attachmentExt(entry.Mime)
			}
			attachments[entry.ID] = entry
			return nil
		}); err == nil {
			s.attachmentsMu.Lock()
			s.attachments = attachments
			s.attachmentsMu.Unlock()
			return
		}
	}
	data, err := os.ReadFile(s.attachmentsIndexPath())
	if err != nil {
		return
	}
	var env struct {
		Attachments []AttachmentEntry `json:"attachments"`
	}
	if json.Unmarshal(data, &env) != nil {
		return
	}
	s.attachmentsMu.Lock()
	for _, entry := range env.Attachments {
		if entry.ID == "" {
			continue
		}
		if entry.Ext == "" {
			entry.Ext = attachmentExt(entry.Mime)
		}
		s.attachments[entry.ID] = entry
	}
	s.attachmentsMu.Unlock()
}

func (s *store) persistAttachmentsLocked() {
	if s.db != nil {
		rows := make(map[string]any, len(s.attachments))
		for id, entry := range s.attachments {
			rows[id] = entry
		}
		if err := s.replaceJSONRows("attachments", "id", rows); err != nil {
			log.Printf("aimebu: persist attachments sqlite: %v", err)
		}
		return
	}
	entries := make([]AttachmentEntry, 0, len(s.attachments))
	for _, entry := range s.attachments {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].UploadedAt < entries[j].UploadedAt
	})
	env := struct {
		Attachments []AttachmentEntry `json:"attachments"`
	}{Attachments: entries}
	if data, err := json.MarshalIndent(env, "", "  "); err == nil {
		atomicWrite(s.attachmentsIndexPath(), data)
	}
}

func (s *store) addAttachment(displayName, mime string, data []byte, width, height int) (types.Attachment, error) {
	if len(data) > maxAttachmentFileSize {
		return types.Attachment{}, fmt.Errorf("file too large (max %d MB)", maxAttachmentFileSize/1024/1024)
	}
	id := randomUUID()
	ext := attachmentExt(mime)
	if err := os.MkdirAll(s.attachmentsDir(), 0o755); err != nil {
		return types.Attachment{}, fmt.Errorf("create attachments dir: %w", err)
	}
	path := filepath.Join(s.attachmentsDir(), id+"."+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return types.Attachment{}, fmt.Errorf("write attachment file: %w", err)
	}
	entry := AttachmentEntry{
		ID:         id,
		Name:       displayName,
		Mime:       mime,
		Size:       int64(len(data)),
		Width:      width,
		Height:     height,
		UploadedAt: now(),
		Ext:        ext,
	}
	s.attachmentsMu.Lock()
	s.attachments[id] = entry
	s.persistAttachmentsLocked()
	s.attachmentsMu.Unlock()
	return attachmentMeta(entry), nil
}

func (s *store) attachmentFilePath(id string) (AttachmentEntry, string, bool) {
	s.attachmentsMu.RLock()
	defer s.attachmentsMu.RUnlock()
	entry, ok := s.attachments[id]
	if !ok {
		return AttachmentEntry{}, "", false
	}
	ext := entry.Ext
	if ext == "" {
		ext = attachmentExt(entry.Mime)
	}
	return entry, filepath.Join(s.attachmentsDir(), id+"."+ext), true
}

func (s *store) resolveAttachments(in []types.Attachment) ([]types.Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxMessageAttachments {
		return nil, fmt.Errorf("too many attachments (max %d)", maxMessageAttachments)
	}
	out := make([]types.Attachment, 0, len(in))
	seen := make(map[string]bool, len(in))
	s.attachmentsMu.RLock()
	defer s.attachmentsMu.RUnlock()
	for _, ref := range in {
		id := strings.TrimSpace(ref.ID)
		if !validUUID(id) {
			return nil, fmt.Errorf("invalid attachment id %q", id)
		}
		if seen[id] {
			continue
		}
		entry, ok := s.attachments[id]
		if !ok {
			return nil, fmt.Errorf("attachment %s not found", id)
		}
		out = append(out, attachmentMeta(entry))
		seen[id] = true
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (s *store) attachmentReferenced(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, roomMessages := range s.messages {
		for _, msg := range roomMessages {
			for _, attachment := range msg.Attachments {
				if attachment.ID == id {
					return true
				}
			}
		}
	}
	return false
}

func (s *store) deleteAttachment(id string) (bool, bool) {
	if !validUUID(id) {
		return false, false
	}
	if s.attachmentReferenced(id) {
		return true, false
	}
	s.attachmentsMu.Lock()
	defer s.attachmentsMu.Unlock()
	entry, ok := s.attachments[id]
	if !ok {
		return false, false
	}
	delete(s.attachments, id)
	ext := entry.Ext
	if ext == "" {
		ext = attachmentExt(entry.Mime)
	}
	_ = os.Remove(filepath.Join(s.attachmentsDir(), id+"."+ext))
	s.persistAttachmentsLocked()
	return true, true
}

func (s *store) cleanupAttachments(now time.Time) {
	referenced := make(map[string]bool)
	s.mu.RLock()
	for _, roomMessages := range s.messages {
		for _, msg := range roomMessages {
			for _, attachment := range msg.Attachments {
				referenced[attachment.ID] = true
			}
		}
	}
	s.mu.RUnlock()

	cutoff := now.Add(-attachmentOrphanGrace)
	changed := false
	s.attachmentsMu.Lock()
	for id, entry := range s.attachments {
		if referenced[id] {
			continue
		}
		uploadedAt, err := time.Parse(time.RFC3339, entry.UploadedAt)
		if err == nil && uploadedAt.After(cutoff) {
			continue
		}
		ext := entry.Ext
		if ext == "" {
			ext = attachmentExt(entry.Mime)
		}
		_ = os.Remove(filepath.Join(s.attachmentsDir(), id+"."+ext))
		delete(s.attachments, id)
		changed = true
	}
	if changed {
		s.persistAttachmentsLocked()
	}
	s.attachmentsMu.Unlock()

	entries, err := os.ReadDir(s.attachmentsDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == attachmentRegistryFile {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		stem := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if !referenced[stem] {
			_, _, known := s.attachmentFilePath(stem)
			if !known {
				_ = os.Remove(filepath.Join(s.attachmentsDir(), entry.Name()))
			}
		}
	}
}

