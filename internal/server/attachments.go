package server

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
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
	if errors.Is(err, store.ErrAttachmentQuota) {
		apiError(w, 429, "attachment_quota", fmt.Sprintf("too many unsent uploads (at most %d, %d MB together): send or remove some first", store.MaxStagedAttachments, store.MaxStagedAttachmentBytes>>20))
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
	// An attachment the person may not open and one that does not exist answer the same, so an id says nothing about whether it exists in a
	// room they are not in.
	notFound := func() { apiError(w, http.StatusNotFound, "not_found", "attachment not found") }
	a, roomID, path, err := s.Store.AttachmentByID(r.Context(), r.PathValue("attachment"), userID(r))
	if errors.Is(err, sql.ErrNoRows) {
		notFound()
		return
	}
	if err != nil && !errors.Is(err, store.ErrAttachmentRemoved) {
		fail(w, err)
		return
	}
	if err := s.Store.UserRoomAccess(r.Context(), userID(r), roomID); errors.Is(err, store.ErrForbidden) || errors.Is(err, sql.ErrNoRows) {
		notFound()
		return
	} else if err != nil {
		fail(w, err)
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

// attachmentSaveWait bounds how long agentAttachmentSave waits for the supervisor, which downloads up to 10 MB and writes it.
var attachmentSaveWait = 2 * time.Minute

var errAttachmentSaveUnsupported = errors.New("the agent's supervisor is too old to save attachments; update it")

// agentAttachmentSave is behind attachment_get when the tool runs in the server (the HTTP MCP endpoint) and the file has to become a file in
// the agent's workspace. The access rule is the download's: an attachment of a note delivered in the caller's live turn, while it is still a
// member of the room. The server then asks the supervisor of the agent's machine to save it (attachment_save with that turn), and answers
// with the path the agent sees.
func (s *Server) agentAttachmentSave(w http.ResponseWriter, r *http.Request) {
	caller := agentToolCaller(r)
	a, _, turnID, _, err := s.Store.AgentTurnAttachment(r.Context(), "", caller.ID, r.PathValue("attachment"))
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, http.StatusNotFound, "not_found", "attachment unavailable")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	machineID, _, err := s.Store.AgentPlacement(r.Context(), caller.ID)
	if err != nil {
		fail(w, err)
		return
	}
	saved, err := s.machineAttachmentSave(r.Context(), machineID, caller.ID, turnID, a.ID)
	switch {
	case errors.Is(err, ErrMachineUnavailable):
		apiError(w, http.StatusServiceUnavailable, "machine_unavailable", "the agent's machine is not connected")
	case errors.Is(err, errAttachmentSaveUnsupported):
		apiError(w, http.StatusConflict, "attachment_save_unsupported", err.Error())
	case err != nil:
		apiError(w, http.StatusBadGateway, "attachment_save_failed", err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"saved": true, "path": saved.Path, "mime": saved.MIME, "size": saved.Size})
	}
}

// machineAttachmentSave sends attachment_save to the machine and waits for its answer.
func (s *Server) machineAttachmentSave(ctx context.Context, machineID, agentID, turnID, attachmentID string) (protocol.AttachmentSaved, error) {
	conn, err := s.machineForID(machineID)
	if err != nil {
		return protocol.AttachmentSaved{}, ErrMachineUnavailable
	}
	if !conn.supportsAttachmentSave {
		return protocol.AttachmentSaved{}, errAttachmentSaveUnsupported
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return protocol.AttachmentSaved{}, err
	}
	id := hex.EncodeToString(nonce[:])
	channel := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	if conn.pathPending == nil {
		conn.pathPending = make(map[string]chan protocol.Frame)
	}
	conn.pathPending[id] = channel
	conn.harnessMu.Unlock()
	defer func() { conn.harnessMu.Lock(); delete(conn.pathPending, id); conn.harnessMu.Unlock() }()
	if err := conn.send(protocol.Frame{V: protocol.Version, Type: protocol.AttachmentSave, ReqID: id, AgentID: agentID, TurnID: turnID, AttachmentID: attachmentID}); err != nil {
		return protocol.AttachmentSaved{}, ErrMachineUnavailable
	}
	timer := time.NewTimer(attachmentSaveWait)
	defer timer.Stop()
	select {
	case frame := <-channel:
		if frame.Code != "" {
			return protocol.AttachmentSaved{}, errors.New(cmp.Or(frame.Message, frame.Code))
		}
		var saved protocol.AttachmentSaved
		if err := json.Unmarshal(frame.Details, &saved); err != nil || saved.Path == "" {
			return protocol.AttachmentSaved{}, errors.New("the machine gave no path")
		}
		return saved, nil
	case <-conn.done:
		return protocol.AttachmentSaved{}, ErrMachineUnavailable
	case <-ctx.Done():
		return protocol.AttachmentSaved{}, ctx.Err()
	case <-timer.C:
		return protocol.AttachmentSaved{}, errors.New("the machine did not answer")
	}
}
