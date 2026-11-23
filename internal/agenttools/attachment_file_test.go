package agenttools

import (
	"bytes"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

// attachmentServer answers the agent attachment route as the server does: the bytes, the type and, for a download, the original name.
func attachmentServer(t *testing.T, contentType, name string, data []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/attachments/att_01TEST" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		if name != "" {
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	return server
}

type toolContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MIMEType string `json:"mimeType"`
}

func callAttachmentGet(t *testing.T, server Server, arguments string) ([]toolContent, bool) {
	t.Helper()
	response, _ := server.Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attachment_get","arguments":`+arguments+`}}`))
	var decoded struct {
		Result struct {
			Content []toolContent `json:"content"`
			IsError bool          `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("decode %s: %v", response, err)
	}
	return decoded.Result.Content, decoded.Result.IsError
}

func savedPath(t *testing.T, content []toolContent) (path string, size int, mimeType string) {
	t.Helper()
	for _, item := range content {
		if item.Type != "text" || !strings.HasPrefix(item.Text, "{") {
			continue
		}
		var saved struct {
			Saved bool   `json:"saved"`
			Path  string `json:"path"`
			MIME  string `json:"mime"`
			Size  int    `json:"size"`
		}
		line, _, _ := strings.Cut(item.Text, "\n")
		if err := json.Unmarshal([]byte(line), &saved); err != nil || !saved.Saved {
			t.Fatalf("saved line %q: %v", item.Text, err)
		}
		return saved.Path, saved.Size, saved.MIME
	}
	t.Fatalf("no saved path in %+v", content)
	return "", 0, ""
}

func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAttachmentGetSavesZipIntoWorkspace(t *testing.T) {
	workdir := realTempDir(t)
	data := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0x00, 0xff, 0x10}, 300<<10)...)
	server := attachmentServer(t, "application/zip", "report bundle.zip", data)
	content, failed := callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test", Workdir: workdir}, `{"attachment_id":"att_01TEST"}`)
	if failed {
		t.Fatalf("failed: %+v", content)
	}
	path, size, mimeType := savedPath(t, content)
	want := filepath.Join(workdir, ".aimebu", "attachments", "att_01TEST", "report bundle.zip")
	if path != want || size != len(data) || mimeType != "application/zip" {
		t.Fatalf("path=%q size=%d mime=%q, want %q %d", path, size, mimeType, want, len(data))
	}
	got, err := os.ReadFile(want)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("saved bytes differ (%d vs %d): %v", len(got), len(data), err)
	}
	info, err := os.Stat(want)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}
	if ignore, err := os.ReadFile(filepath.Join(workdir, ".aimebu", ".gitignore")); err != nil || string(ignore) != "*\n" {
		t.Fatalf(".aimebu/.gitignore = %q, %v", ignore, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(want))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
	// Asking again replaces the copy rather than failing.
	if _, failed := callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test", Workdir: workdir}, `{"attachment_id":"att_01TEST"}`); failed {
		t.Fatal("second save failed")
	}
}

func TestAttachmentGetRefusesOversizeAttachment(t *testing.T) {
	workdir := realTempDir(t)
	server := attachmentServer(t, "application/zip", "big.zip", bytes.Repeat([]byte{1}, MaxAttachmentBytes+1))
	content, failed := callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test", Workdir: workdir}, `{"attachment_id":"att_01TEST"}`)
	if !failed || len(content) != 1 || !strings.Contains(content[0].Text, "larger than 10 MB") {
		t.Fatalf("failed=%v content=%+v", failed, content)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".aimebu")); !os.IsNotExist(err) {
		t.Fatalf("oversize attachment touched the workspace: %v", err)
	}
}

func TestAttachmentGetKeepsImageAndSmallTextInline(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, file string
		data                    []byte
	}{
		{"image", "image/png", "", []byte("\x89PNG\r\n\x1a\npixels")},
		{"small text", "text/plain", "note.txt", []byte("small note")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workdir := realTempDir(t)
			server := attachmentServer(t, tc.contentType, tc.file, tc.data)
			content, failed := callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test", Workdir: workdir}, `{"attachment_id":"att_01TEST"}`)
			if failed || len(content) != 1 {
				t.Fatalf("failed=%v content=%+v", failed, content)
			}
			if tc.contentType == "image/png" && (content[0].Type != "image" || content[0].MIMEType != "image/png") || tc.contentType == "text/plain" && content[0].Text != "small note" {
				t.Fatalf("content=%+v", content)
			}
			if _, err := os.Stat(filepath.Join(workdir, ".aimebu")); !os.IsNotExist(err) {
				t.Fatalf("inline attachment was saved: %v", err)
			}

			// save:true keeps the inline content and also saves the file.
			content, failed = callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test", Workdir: workdir}, `{"attachment_id":"att_01TEST","save":true}`)
			wantType := "text"
			if tc.contentType == "image/png" {
				wantType = "image"
			}
			if failed || len(content) != 2 || content[0].Type != wantType {
				t.Fatalf("failed=%v content=%+v", failed, content)
			}
			path, _, _ := savedPath(t, content)
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, tc.data) {
				t.Fatalf("saved %q: %v", got, err)
			}
			if tc.file == "" && filepath.Base(path) != "attachment" {
				t.Fatalf("an image without a name is saved as %q", path)
			}
		})
	}
}

// Without a workspace of its own (the server's HTTP endpoint) the tool asks the server to have the agent's supervisor save the file, and
// passes on the path, or the reason it could not be saved.
func TestAttachmentGetWithoutWorkspaceAsksTheServerToSave(t *testing.T) {
	saveAnswer := `{"saved":true,"path":"/workspace/.aimebu/attachments/att_01TEST/spec.pdf","mime":"application/pdf","size":8}`
	saveStatus := http.StatusOK
	saves := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/agent/attachments/att_01TEST":
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", `attachment; filename="spec.pdf"`)
			_, _ = w.Write([]byte("%PDF-1.7"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent/attachments/att_01TEST/save" && r.Header.Get("Authorization") == "Bearer aak_test":
			saves++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(saveStatus)
			_, _ = w.Write([]byte(saveAnswer))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	content, failed := callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test"}, `{"attachment_id":"att_01TEST"}`)
	if failed || saves != 1 {
		t.Fatalf("failed=%v saves=%d content=%+v", failed, saves, content)
	}
	if path, size, mimeType := savedPath(t, content); path != "/workspace/.aimebu/attachments/att_01TEST/spec.pdf" || size != 8 || mimeType != "application/pdf" {
		t.Fatalf("saved %q %d %q", path, size, mimeType)
	}

	saveStatus, saveAnswer = http.StatusConflict, `{"error":"attachment_save_unsupported","message":"the agent's supervisor is too old to save attachments; update it"}`
	content, failed = callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test"}, `{"attachment_id":"att_01TEST"}`)
	if failed || len(content) != 1 || !strings.Contains(content[0].Text, "cannot be saved to your workspace here: the agent's supervisor is too old") || !strings.Contains(content[0].Text, "application/pdf, 8 bytes") {
		t.Fatalf("failed=%v content=%+v", failed, content)
	}
	content, failed = callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test"}, `{"attachment_id":"att_01TEST","save":"yes"}`)
	if !failed || !strings.Contains(content[0].Text, "save must be a boolean") {
		t.Fatalf("failed=%v content=%+v", failed, content)
	}
}

func TestAttachmentGetRefusesSymlinkedParent(t *testing.T) {
	for _, link := range []string{".aimebu", ".aimebu/attachments", ".aimebu/attachments/att_01TEST"} {
		t.Run(link, func(t *testing.T) {
			workdir, outside := realTempDir(t), realTempDir(t)
			if parent := filepath.Dir(filepath.Join(workdir, link)); parent != workdir {
				if err := os.MkdirAll(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(workdir, link)); err != nil {
				t.Fatal(err)
			}
			server := attachmentServer(t, "application/zip", "x.zip", []byte("PK"))
			content, failed := callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test", Workdir: workdir}, `{"attachment_id":"att_01TEST"}`)
			if !failed || !strings.Contains(content[0].Text, "could not be saved") {
				t.Fatalf("failed=%v content=%+v", failed, content)
			}
			var written []string
			_ = filepath.WalkDir(outside, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					written = append(written, path)
				}
				return nil
			})
			if len(written) != 0 {
				t.Fatalf("wrote through the link: %v", written)
			}
		})
	}
}

func TestSaveAttachmentRejectsBadInput(t *testing.T) {
	workdir := realTempDir(t)
	for _, tc := range []struct{ workdir, id string }{
		{workdir, "att_../../x"},
		{workdir, "att_a/b"},
		{workdir, "att_"},
		{"relative/dir", "att_01TEST"},
		{"", "att_01TEST"},
	} {
		if path, err := SaveAttachment(tc.workdir, tc.id, "x.zip", []byte("PK")); err == nil {
			t.Fatalf("SaveAttachment(%q, %q) = %q", tc.workdir, tc.id, path)
		}
	}
}

func TestSanitizeAttachmentName(t *testing.T) {
	long := strings.Repeat("a", 300) + ".zip"
	longUnicode := strings.Repeat("ž", 150) + ".tar.gz"
	for _, tc := range []struct{ in, want string }{
		{"report.zip", "report.zip"},
		{"../x", "x"},
		{"..", "attachment"},
		{".", "attachment"},
		{"/etc/passwd", "passwd"},
		{"a/b/c.txt", "c.txt"},
		{`C:\Users\x\evil.exe`, "evil.exe"},
		{"dir/", "attachment"},
		{"", "attachment"},
		{"   ", "attachment"},
		{".bashrc", "bashrc"},
		{"...", "attachment"},
		{"bad\x00name\n.txt", "badname.txt"},
		{"tab\tand\x7fdel", "tabanddel"},
		{"rtl\u202egnp.exe", "rtlgnp.exe"},
		{"zpráva – přehled.pdf", "zpráva – přehled.pdf"},
		{"日本語のファイル.txt", "日本語のファイル.txt"},
		{"bad\xffutf8.bin", "bad_utf8.bin"},
		{"trailing dot. ", "trailing dot"},
	} {
		if got := sanitizeAttachmentName(tc.in); got != tc.want {
			t.Errorf("sanitizeAttachmentName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, in := range []string{long, longUnicode} {
		got := sanitizeAttachmentName(in)
		if len(got) > maxSavedNameBytes || !utf8.ValidString(got) || filepath.Ext(got) != filepath.Ext(in) || strings.ContainsAny(got, `/\`) {
			t.Errorf("long name %d bytes -> %q (%d bytes)", len(in), got, len(got))
		}
	}
}

// A file saved for another user (a container's) takes the checkout's read access for group and others, and never more.
func TestSaveAttachmentLikeCheckoutCopiesTheCheckoutsReadAccess(t *testing.T) {
	for _, tc := range []struct {
		checkout       os.FileMode
		wantDir, wantF os.FileMode
	}{
		{0755, 0755, 0644},
		{0750, 0750, 0640},
		{0700, 0700, 0600},
		{0777, 0755, 0644}, // write access is never handed on
	} {
		workdir := realTempDir(t)
		if err := os.Chmod(workdir, tc.checkout); err != nil {
			t.Fatal(err)
		}
		path, err := SaveAttachmentLikeCheckout(workdir, "att_01TEST", "x.zip", []byte("PK"))
		if err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
			if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != tc.wantDir {
				t.Errorf("checkout %v: %s mode %v, want %v (%v)", tc.checkout, dir, info.Mode().Perm(), tc.wantDir, err)
			}
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != tc.wantF {
			t.Errorf("checkout %v: file mode %v, want %v (%v)", tc.checkout, info.Mode().Perm(), tc.wantF, err)
		}
		// .aimebu/ can be passed through as far as the checkout can, never listed by others.
		if info, err := os.Stat(filepath.Join(workdir, ".aimebu")); err != nil || info.Mode().Perm() != 0700|tc.checkout&0011 {
			t.Errorf("checkout %v: .aimebu mode %v (%v)", tc.checkout, info.Mode().Perm(), err)
		}
	}
	// The plain save stays private whatever the checkout.
	workdir := realTempDir(t)
	if err := os.Chmod(workdir, 0755); err != nil {
		t.Fatal(err)
	}
	path, err := SaveAttachment(workdir, "att_01TEST", "x.zip", []byte("PK"))
	if info, statErr := os.Stat(path); err != nil || statErr != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("plain save: %v %v", err, statErr)
	}
}

// A checkout owned by another user (the container's) hands that owner the folders and the file; a process that may not give files away
// still saves them.
func TestSaveAttachmentLikeCheckoutGivesTheCheckoutsOwner(t *testing.T) {
	origOwner, origChown := checkoutOwner, chownPath
	t.Cleanup(func() { checkoutOwner, chownPath = origOwner, origChown })
	uid, gid := os.Geteuid()+1000, os.Getegid()+1000
	checkoutOwner = func(os.FileInfo) (int, int, bool) { return uid, gid, true }
	for _, refuse := range []bool{false, true} {
		var owned []string
		chownPath = func(path string, u, g int) error {
			if u != uid || g != gid {
				t.Errorf("chown %s to %d:%d, want %d:%d", path, u, g, uid, gid)
			}
			owned = append(owned, filepath.Base(path))
			if refuse {
				return &os.PathError{Op: "lchown", Path: path, Err: syscall.EPERM}
			}
			return nil
		}
		workdir := realTempDir(t)
		path, err := SaveAttachmentLikeCheckout(workdir, "att_01TEST", "x.zip", []byte("PK"))
		if err != nil {
			t.Fatalf("refuse=%v: %v", refuse, err)
		}
		if len(owned) != 3 || owned[0] != "attachments" || owned[1] != "att_01TEST" || !strings.HasPrefix(owned[2], ".partial-") {
			t.Fatalf("refuse=%v: chowned %v", refuse, owned)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "PK" {
			t.Fatalf("refuse=%v: %q %v", refuse, got, err)
		}
	}
	// The same owner as this process: nothing to give away.
	checkoutOwner = func(os.FileInfo) (int, int, bool) { return os.Geteuid(), os.Getegid(), true }
	chownPath = func(path string, _, _ int) error {
		t.Errorf("chown %s for the process's own checkout", path)
		return nil
	}
	if _, err := SaveAttachmentLikeCheckout(realTempDir(t), "att_01TEST", "x.zip", []byte("PK")); err != nil {
		t.Fatal(err)
	}
}

// Over the server (no workspace of its own) a file that cannot come back inline is saved from a HEAD alone: the bytes are downloaded once,
// by the supervisor, not here first. A file that may come back inline is still downloaded.
func TestAttachmentGetWithoutWorkspaceDecidesFromHead(t *testing.T) {
	var gets, heads, saves atomic.Int64
	contentType, size := "application/zip", "2400000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/agent/attachments/att_01TEST" && (r.Method == http.MethodHead || r.Method == http.MethodGet):
			if r.Method == http.MethodHead {
				heads.Add(1)
			} else {
				gets.Add(1)
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", size)
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("small note"))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/agent/attachments/att_01TEST/save":
			saves.Add(1)
			_, _ = w.Write([]byte(`{"saved":true,"path":"/workspace/.aimebu/attachments/att_01TEST/a.zip","mime":"application/zip","size":2400000}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	content, failed := callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test"}, `{"attachment_id":"att_01TEST"}`)
	if failed || heads.Load() != 1 || gets.Load() != 0 || saves.Load() != 1 {
		t.Fatalf("zip: failed=%v heads=%d gets=%d saves=%d content=%+v", failed, heads.Load(), gets.Load(), saves.Load(), content)
	}
	if path, n, _ := savedPath(t, content); path != "/workspace/.aimebu/attachments/att_01TEST/a.zip" || n != 2400000 {
		t.Fatalf("saved %q %d", path, n)
	}

	size = strconv.Itoa(MaxAttachmentBytes + 1)
	content, failed = callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test"}, `{"attachment_id":"att_01TEST"}`)
	if !failed || gets.Load() != 0 || saves.Load() != 1 || !strings.Contains(content[0].Text, "larger than 10 MB") {
		t.Fatalf("oversize: failed=%v gets=%d saves=%d content=%+v", failed, gets.Load(), saves.Load(), content)
	}

	contentType, size = "text/plain", "10"
	content, failed = callAttachmentGet(t, Server{URL: server.URL, Key: "aak_test"}, `{"attachment_id":"att_01TEST"}`)
	if failed || gets.Load() != 1 || saves.Load() != 1 || len(content) != 1 || content[0].Text != "small note" {
		t.Fatalf("small text: failed=%v gets=%d saves=%d content=%+v", failed, gets.Load(), saves.Load(), content)
	}
}
