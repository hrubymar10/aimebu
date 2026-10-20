package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

var (
	testWAV = append([]byte("RIFF\x24\x00\x00\x00WAVE"), make([]byte, 32)...)
	testMP3 = append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), make([]byte, 32)...)
)

func uploadSoundAs(t *testing.T, f *fixture, cookie *http.Cookie, csrf, filename string, data []byte) (*http.Response, soundView) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(data)
	form.Close()
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/sounds", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var view soundView
	if resp.StatusCode == 201 {
		json.NewDecoder(resp.Body).Decode(&view)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, view
}

func listSounds(t *testing.T, f *fixture, cookie *http.Cookie) []soundView {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.http.URL+"/api/sounds", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Sounds []soundView `json:"sounds"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Sounds
}

func TestSoundsUploadListServeDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if sounds := listSounds(t, f, f.cookie); len(sounds) != 4 || sounds[0].ID != "builtin:chime" || sounds[3].Kind != "builtin" {
		t.Fatalf("fresh install should list only the bundled sounds: %+v", sounds)
	}
	resp, mp3 := uploadSoundAs(t, f, f.cookie, f.csrf, "../../etc/My Ping.MP3", testMP3)
	if resp.StatusCode != 201 || mp3.Name != "My Ping.MP3" || mp3.Ext != "mp3" || !strings.HasPrefix(mp3.ID, "user:snd_") {
		t.Fatalf("mp3 upload: %d %+v", resp.StatusCode, mp3)
	}
	if resp, wav := uploadSoundAs(t, f, f.cookie, f.csrf, "knock2.wav", testWAV); resp.StatusCode != 201 || wav.Ext != "wav" {
		t.Fatalf("wav upload: %d %+v", resp.StatusCode, wav)
	}
	if sounds := listSounds(t, f, f.cookie); len(sounds) != 6 || sounds[4].ID != mp3.ID {
		t.Fatalf("uploads should follow the bundled sounds in upload order: %+v", sounds)
	}
	serve := f.request(http.MethodGet, "/api/sounds/"+strings.TrimPrefix(mp3.ID, "user:"), nil, false)
	data, _ := io.ReadAll(serve.Body)
	serve.Body.Close()
	if serve.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("sounds must not be cached: %q", serve.Header.Get("Cache-Control"))
	}
	if serve.StatusCode != 200 || serve.Header.Get("Content-Type") != "audio/mpeg" || serve.Header.Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(data, testMP3) {
		t.Fatalf("serve: %d %q %q", serve.StatusCode, serve.Header.Get("Content-Type"), data)
	}
	_, path, err := f.db.SoundFile(t.Context(), strings.TrimPrefix(mp3.ID, "user:"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("sound file should be private: %v %v", info, err)
	}
	del := f.request(http.MethodDelete, "/api/sounds/"+strings.TrimPrefix(mp3.ID, "user:"), nil, true)
	del.Body.Close()
	if del.StatusCode != 204 {
		t.Fatalf("delete: %d", del.StatusCode)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("delete should remove the file: %v", err)
	}
	gone := f.request(http.MethodGet, "/api/sounds/"+strings.TrimPrefix(mp3.ID, "user:"), nil, false)
	gone.Body.Close()
	missing := f.request(http.MethodDelete, "/api/sounds/"+strings.TrimPrefix(mp3.ID, "user:"), nil, true)
	missing.Body.Close()
	if gone.StatusCode != 404 || missing.StatusCode != 404 {
		t.Fatalf("deleted sound should be gone: %d %d", gone.StatusCode, missing.StatusCode)
	}
}

func TestSoundsUploadRejectsBadFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cases := []struct {
		name, file string
		data       []byte
	}{
		{"wrong extension", "a.ogg", testWAV},
		{"wav bytes named mp3", "a.mp3", testWAV},
		{"mp3 bytes named wav", "a.wav", testMP3},
		{"empty", "a.wav", nil},
	}
	for _, tc := range cases {
		if resp, _ := uploadSoundAs(t, f, f.cookie, f.csrf, tc.file, tc.data); resp.StatusCode != 400 {
			t.Errorf("%s: got %d, want 400", tc.name, resp.StatusCode)
		}
	}
	// Oversize is 413, both just over the file cap and far over the body cap.
	for _, extra := range []int{store.MaxSoundFileSize, 4 * store.MaxSoundFileSize} {
		big := append(append([]byte{}, testWAV...), make([]byte, extra)...)
		if resp, _ := uploadSoundAs(t, f, f.cookie, f.csrf, "big.wav", big); resp.StatusCode != 413 {
			t.Errorf("%d extra bytes: got %d, want 413", extra, resp.StatusCode)
		}
	}
	// A body that is not multipart at all is a 400, not a size error.
	bad, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/sounds", strings.NewReader("not multipart"))
	bad.Header.Set("Content-Type", "text/plain")
	bad.Header.Set("X-CSRF-Token", f.csrf)
	bad.AddCookie(f.cookie)
	if resp, err := http.DefaultClient.Do(bad); err != nil || resp.StatusCode != 400 {
		t.Errorf("malformed upload: %v %v", resp, err)
	}
	if sounds := listSounds(t, f, f.cookie); len(sounds) != 4 {
		t.Fatalf("rejected uploads must not be stored: %+v", sounds)
	}
}

func TestSoundsAreOwnerManagedAndCSRFProtected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, own := uploadSoundAs(t, f, f.cookie, f.csrf, "mine.wav", testWAV)
	cookie, csrf := registerSecondUser(t, f, "guest")
	if resp, _ := uploadSoundAs(t, f, cookie, csrf, "guest.wav", testWAV); resp.StatusCode != 403 {
		t.Fatalf("non-owner upload: %d", resp.StatusCode)
	}
	id := strings.TrimPrefix(own.ID, "user:")
	if resp := f.requestAs(t, cookie, csrf, http.MethodDelete, "/api/sounds/"+id, nil); resp.StatusCode != 403 {
		t.Fatalf("non-owner delete: %d", resp.StatusCode)
	}
	if sounds := listSounds(t, f, cookie); len(sounds) != 5 {
		t.Fatalf("any signed-in user may list: %+v", sounds)
	}
	if resp := f.requestAs(t, cookie, csrf, http.MethodGet, "/api/sounds/"+id, nil); resp.StatusCode != 200 {
		t.Fatalf("any signed-in user may play: %d", resp.StatusCode)
	}
	if resp := f.request(http.MethodDelete, "/api/sounds/"+id, nil, false); resp.StatusCode != 403 && resp.StatusCode != 400 {
		t.Fatalf("delete without a CSRF token should be refused: %d", resp.StatusCode)
	}
	anonymous, _ := http.Get(f.http.URL + "/api/sounds")
	anonymous.Body.Close()
	if anonymous.StatusCode != 401 {
		t.Fatalf("anonymous list: %d", anonymous.StatusCode)
	}
}

func TestSoundsEnforceCountLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var dir string
	for i := 0; i < store.MaxSoundFiles; i++ {
		resp, view := uploadSoundAs(t, f, f.cookie, f.csrf, "s.wav", testWAV)
		if resp.StatusCode != 201 {
			t.Fatalf("upload %d: %d", i, resp.StatusCode)
		}
		if dir == "" {
			_, path, err := f.db.SoundFile(t.Context(), strings.TrimPrefix(view.ID, "user:"))
			if err != nil {
				t.Fatal(err)
			}
			dir = filepath.Dir(path)
		}
	}
	if resp, _ := uploadSoundAs(t, f, f.cookie, f.csrf, "one-too-many.wav", testWAV); resp.StatusCode != 409 {
		t.Fatalf("eleventh upload: %d", resp.StatusCode)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != store.MaxSoundFiles {
		t.Fatalf("rejected upload left a file behind: %d files", len(entries))
	}
}
