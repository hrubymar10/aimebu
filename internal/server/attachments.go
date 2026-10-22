package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/store"
)

func attachmentBearer(keyHash, turnID string) string {
	mac := hmac.New(sha256.New, []byte(keyHash))
	mac.Write([]byte("aimebu2-attachment-v1\x00"))
	mac.Write([]byte(turnID))
	return "aat_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if !s.roomWriteAccess(w, r, room) {
		return
	}
	if room.Lifecycle != "open" {
		apiError(w, 409, "room_closed", "room is not open")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxAttachmentSize+(1<<20))
	if err := r.ParseMultipartForm(store.MaxAttachmentSize); err != nil {
		apiError(w, 413, "invalid_attachment", "file exceeds 10 MB")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		apiError(w, 400, "invalid_attachment", "file required")
		return
	}
	defer file.Close()
	a, err := s.Store.SaveAttachment(r.Context(), room.ID, userID(r), header.Filename, file)
	if errors.Is(err, store.ErrInvalidAttachment) {
		apiError(w, 400, "invalid_attachment", "nonempty file up to 10 MB required")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, a)
}

func (s *Server) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if !s.roomWriteAccess(w, r, room) {
		return
	}
	if err := s.Store.RemoveStagedAttachment(r.Context(), r.PathValue("attachment"), room.ID, userID(r)); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) downloadAttachment(w http.ResponseWriter, r *http.Request) {
	a, roomID, path, err := s.Store.AttachmentByID(r.Context(), r.PathValue("attachment"))
	if err != nil && !errors.Is(err, store.ErrAttachmentRemoved) {
		fail(w, err)
		return
	}
	if _, ok := s.roomAccess(w, r, roomID); !ok {
		return
	}
	if errors.Is(err, store.ErrAttachmentRemoved) {
		apiError(w, 410, "attachment_removed", fmt.Sprintf("this attachment was removed after %d days", a.RemovedAfterDays))
		return
	}
	serveAttachment(w, r, a, path)
}

// turnAttachment accepts a capability scoped to one live turn. Pi Docker uses
// this route because its launcher has no private MCP credential channel.
func (s *Server) turnAttachment(w http.ResponseWriter, r *http.Request) {
	a, path, turnID, keyHash, err := s.Store.AgentTurnAttachment(r.Context(), r.PathValue("turn"), "", r.PathValue("attachment"))
	if err != nil {
		apiError(w, 404, "not_found", "attachment unavailable")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	expected := attachmentBearer(keyHash, turnID)
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		apiError(w, 404, "not_found", "attachment unavailable")
		return
	}
	serveAttachment(w, r, a, path)
}

// agentAttachment is the binary endpoint behind the attachment_get MCP tool.
func (s *Server) agentAttachment(w http.ResponseWriter, r *http.Request) {
	a, path, _, _, err := s.Store.AgentTurnAttachment(r.Context(), "", agentToolCaller(r).ID, r.PathValue("attachment"))
	if err != nil {
		fail(w, err)
		return
	}
	serveAttachment(w, r, a, path)
}

func serveAttachment(w http.ResponseWriter, r *http.Request, a store.Attachment, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "private, no-store")
	inlineImage := false
	switch a.MIME {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		inlineImage = true
	case "image/svg+xml":
		// An SVG is an image only inside an <img> (no script runs there and nothing outside the file is fetched). A request for anything
		// else (opening the address in a tab, a download link, a client that sends no Sec-Fetch-Dest) gets it as a download, so the
		// file can never be shown as a document of this origin.
		inlineImage = r.Header.Get("Sec-Fetch-Dest") == "image"
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	}
	if !inlineImage {
		name := a.Name
		if name == "" {
			name = "attachment"
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	http.ServeContent(w, r, a.ID, fStatTime(f), f)
}

func fStatTime(f *os.File) (t time.Time) {
	info, err := f.Stat()
	if err == nil {
		return info.ModTime()
	}
	return
}
