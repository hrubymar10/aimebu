package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const MaxAttachmentSize = 10 << 20
const MaxMessageAttachments = 4

var ErrInvalidAttachment = errors.New("invalid_attachment")

type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
	Path string `json:"path,omitempty"`
	// Width and Height are the pixel size of an image, when known: a thumbnail can hold its place before it loads.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// RemovedAt is set when retention removed the file; the message keeps this record, and the chat shows that the attachment was
	// removed after RemovedAfterDays days instead of the file.
	RemovedAt        string `json:"removed_at,omitempty"`
	RemovedAfterDays int    `json:"removed_after_days,omitempty"`
}

// ErrAttachmentRemoved is returned for an attachment whose file retention has removed.
var ErrAttachmentRemoved = errors.New("attachment removed")

func safeAttachmentName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	runes := []rune(name)
	if len(runes) > 200 {
		name = string(runes[:200])
	}
	return name
}

// SaveAttachment stages a file for one room. The room is fixed at upload,
// so neither a guessed ID nor a later message can move it across rooms.
func (s *Store) SaveAttachment(ctx context.Context, roomID, userID, filename string, input io.Reader) (Attachment, error) {
	if err := s.UserRoomAccess(ctx, userID, roomID); err != nil {
		return Attachment{}, err
	}
	data, err := io.ReadAll(io.LimitReader(input, MaxAttachmentSize+1))
	if err != nil {
		return Attachment{}, err
	}
	if len(data) == 0 || len(data) > MaxAttachmentSize {
		return Attachment{}, ErrInvalidAttachment
	}
	mime := http.DetectContentType(data)
	// Go does not sniff SVG (it reads as plain text or XML). A file that is plainly a still image becomes image/svg+xml; any other keeps its
	// sniffed type and stays a download (see ClassifySVG).
	svg := ClassifySVG(data)
	if svg.Image {
		mime = "image/svg+xml"
	}
	name := safeAttachmentName(filename)
	id, err := newID("att")
	if err != nil {
		return Attachment{}, err
	}
	if err := os.MkdirAll(s.attachmentDir, 0700); err != nil {
		return Attachment{}, err
	}
	file, err := os.CreateTemp(s.attachmentDir, ".upload-*")
	if err != nil {
		return Attachment{}, err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return Attachment{}, err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return Attachment{}, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return Attachment{}, err
	}
	if err := file.Close(); err != nil {
		return Attachment{}, err
	}
	path := filepath.Join(s.attachmentDir, id+".bin")
	if err := os.Rename(file.Name(), path); err != nil {
		return Attachment{}, err
	}
	hash := sha256.Sum256(data)
	result := Attachment{ID: id, Name: name, MIME: mime, Size: int64(len(data)), URL: "/api/attachments/" + id}
	if svg.Image {
		result.Width, result.Height = svg.Width, svg.Height
	} else if strings.HasPrefix(mime, "image/") {
		if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			result.Width, result.Height = config.Width, config.Height
		}
	}
	err = s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id=?`, roomID).Scan(&projectID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO attachment(id,project_id,room_id,sha256,mime,size,path,created_by,created_at,filename,width,height,arrived_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, projectID, roomID, hex.EncodeToString(hash[:]), mime, len(data), path, userID, timestamp(), name, result.Width, result.Height, timestamp())
		return err
	})
	if err != nil {
		os.Remove(path)
		return Attachment{}, err
	}
	return result, nil
}

func (s *Store) AttachmentByID(ctx context.Context, id string) (Attachment, string, string, error) {
	var a Attachment
	var roomID, path string
	var removedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,room_id,filename,mime,size,path,width,height,removed_at,removed_after_days FROM attachment WHERE id=? AND room_id IS NOT NULL`, id).
		Scan(&a.ID, &roomID, &a.Name, &a.MIME, &a.Size, &path, &a.Width, &a.Height, &removedAt, &a.RemovedAfterDays)
	a.URL = "/api/attachments/" + a.ID
	a.RemovedAt = removedAt.String
	if err == nil && removedAt.Valid {
		return a, roomID, path, ErrAttachmentRemoved
	}
	return a, roomID, path, err
}

func (s *Store) AttachmentsForMessage(ctx context.Context, messageID string) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,filename,mime,size,width,height,COALESCE(removed_at,''),removed_after_days FROM attachment WHERE message_id=? ORDER BY created_at,id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Attachment
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.Name, &a.MIME, &a.Size, &a.Width, &a.Height, &a.RemovedAt, &a.RemovedAfterDays); err != nil {
			return nil, err
		}
		a.URL = "/api/attachments/" + a.ID
		result = append(result, a)
	}
	return result, rows.Err()
}

func (tx *Tx) bindAttachments(message Message) ([]Attachment, error) {
	if len(message.AttachmentIDs) > MaxMessageAttachments {
		return nil, ErrInvalidAttachment
	}
	seen := map[string]bool{}
	var result []Attachment
	for _, id := range message.AttachmentIDs {
		if seen[id] || !strings.HasPrefix(id, "att_") {
			return nil, ErrInvalidAttachment
		}
		seen[id] = true
		var a Attachment
		var roomID, creator string
		var bound sql.NullString
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT room_id,created_by,message_id,filename,mime,size,width,height FROM attachment WHERE id=?`, id).Scan(&roomID, &creator, &bound, &a.Name, &a.MIME, &a.Size, &a.Width, &a.Height)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrInvalidAttachment
			}
			return nil, err
		}
		if roomID != message.RoomID || bound.Valid || (message.AuthorKind == "user" && creator != message.AuthorID) {
			return nil, ErrInvalidAttachment
		}
		if _, err := tx.Exec(`UPDATE attachment SET message_id=? WHERE id=?`, message.ID, id); err != nil {
			return nil, err
		}
		a.ID = id
		a.URL = "/api/attachments/" + id
		result = append(result, a)
	}
	return result, nil
}

// AgentTurnAttachment returns a blob only while the specified turn can still
// use it and its agent remains a member of the attachment's room.
func (s *Store) AgentTurnAttachment(ctx context.Context, turnID, agentID, attachmentID string) (Attachment, string, string, string, error) {
	var a Attachment
	var path, actualTurnID, keyHash string
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.filename,a.mime,a.size,a.path,t.id,g.key_hash FROM attachment a
		JOIN note n ON n.message_id=a.message_id AND n.room_id=a.room_id
		JOIN turn_note tn ON tn.note_id=n.id
		JOIN turn t ON t.id=tn.turn_id AND t.agent_id=n.agent_id
		JOIN agent g ON g.id=t.agent_id AND g.revoked_at IS NULL
		JOIN room_member own ON own.room_id=a.room_id AND own.member_kind='agent'
			AND own.member_id=t.agent_id AND own.left_at IS NULL
		WHERE a.id=? AND a.removed_at IS NULL AND (?='' OR t.id=?) AND (?='' OR t.agent_id=?)
		AND t.exec IN ('pending','running') AND a.message_id IS NOT NULL
		AND g.key_hash!='' LIMIT 1`, attachmentID, turnID, turnID, agentID, agentID).
		Scan(&a.ID, &a.Name, &a.MIME, &a.Size, &path, &actualTurnID, &keyHash)
	a.URL = "/api/attachments/" + a.ID
	return a, path, actualTurnID, keyHash, err
}

func (s *Store) RemoveStagedAttachment(ctx context.Context, id, roomID, userID string) error {
	var path string
	err := s.Tx(ctx, func(tx *Tx) error {
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT path FROM attachment WHERE id=? AND room_id=? AND created_by=? AND message_id IS NULL`, id, roomID, userID).Scan(&path); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM attachment WHERE id=?`, id)
		return err
	})
	if err == nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("remove attachment: %w", removeErr)
		}
	}
	return err
}

// SweepStagedAttachments removes uploads never bound to a message after a day.
func (s *Store) SweepStagedAttachments(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(timestampLayout)
	var paths []string
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT path FROM attachment WHERE message_id IS NULL AND created_at<?`, cutoff)
		if err != nil {
			return err
		}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return err
			}
			paths = append(paths, path)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM attachment WHERE message_id IS NULL AND created_at<?`, cutoff)
		return err
	})
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
