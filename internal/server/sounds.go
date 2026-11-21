package server

import (
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// builtinSounds are the bundled notification sounds served from /sounds/.
var builtinSounds = []struct{ ID, Name string }{
	{"builtin:chime", "Chime"}, {"builtin:ding", "Ding"}, {"builtin:beep", "Beep"}, {"builtin:knock", "Knock"},
}

type soundView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size,omitempty"`
	UploadedAt string `json:"uploaded_at,omitempty"`
	Ext        string `json:"ext,omitempty"`
}

func userSoundView(sound store.Sound) soundView {
	return soundView{ID: "user:" + sound.ID, Name: sound.Name, Kind: "user", Size: sound.Size, UploadedAt: sound.UploadedAt, Ext: sound.Ext}
}

// sounds lists the bundled sounds followed by the owner's uploads.
func (s *Server) sounds(w http.ResponseWriter, r *http.Request) {
	list := make([]soundView, 0, len(builtinSounds)+store.MaxSoundFiles)
	for _, b := range builtinSounds {
		list = append(list, soundView{ID: b.ID, Name: b.Name, Kind: "builtin"})
	}
	uploads, err := s.Store.ListSounds(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	for _, sound := range uploads {
		list = append(list, userSoundView(sound))
	}
	writeJSON(w, 200, map[string]any{"sounds": list})
}

// uploadSound accepts one multipart "file" (.mp3 or .wav, up to 1 MB). Owner only.
func (s *Server) uploadSound(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxSoundFileSize+(64<<10))
	if err := r.ParseMultipartForm(store.MaxSoundFileSize + (64 << 10)); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apiError(w, 413, "invalid_sound", "sound exceeds 1 MB")
		} else {
			apiError(w, 400, "invalid_sound", "malformed multipart upload")
		}
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		apiError(w, 400, "invalid_sound", "file required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, store.MaxSoundFileSize+1))
	if err != nil {
		apiError(w, 400, "invalid_sound", "could not read the upload")
		return
	}
	if len(data) > store.MaxSoundFileSize {
		apiError(w, 413, "invalid_sound", "sound exceeds 1 MB")
		return
	}
	sound, err := s.Store.SaveSound(r.Context(), userID(r), header.Filename, data)
	switch {
	case errors.Is(err, store.ErrInvalidSound):
		apiError(w, 400, "invalid_sound", "a valid .mp3 or .wav file up to 1 MB is required")
	case errors.Is(err, store.ErrSoundLimit):
		apiError(w, 409, "sound_limit", err.Error())
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, 201, userSoundView(sound))
	}
}

func (s *Server) deleteSound(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	if err := s.Store.DeleteSound(r.Context(), r.PathValue("sound")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) downloadSound(w http.ResponseWriter, r *http.Request) {
	sound, path, err := s.Store.SoundFile(r.Context(), r.PathValue("sound"))
	if err != nil {
		fail(w, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		apiError(w, 404, "not_found", "not found")
		return
	}
	defer f.Close()
	contentType := "audio/mpeg"
	if sound.Ext == "wav" {
		contentType = "audio/wav"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, sound.ID, fStatTime(f), f)
}
