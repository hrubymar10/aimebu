package server

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/agenttools"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

var testPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL/nwAAAABJRU5ErkJggg==")

func uploadFile(t *testing.T, f *fixture, roomID, filename string, data []byte) (*http.Response, store.Attachment) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+roomID+"/attachments", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var a store.Attachment
	if resp.StatusCode == 201 {
		if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	return resp, a
}

// ownerUserID is the id of the fixture's owner account: created_by holds ids, not logins, so a test that asks for a draft passes this.
func (f *fixture) ownerUserID(t *testing.T) string {
	t.Helper()
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	return owner.ID
}

func uploadImage(t *testing.T, f *fixture, roomID string, data []byte) (*http.Response, store.Attachment) {
	return uploadFile(t, f, roomID, "image.png", data)
}

func TestAttachmentRoomBindingAndPrivateRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp, a := uploadImage(t, f, f.room.ID, testPNG)
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	other, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "task", Title: "other", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodPost, "/api/rooms/"+other.ID+"/messages", map[string]any{"body": "cross room", "attachments": []string{a.ID}}, true)
	if resp.StatusCode != 400 {
		t.Fatalf("cross-room bind: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]any{"body": "", "attachments": []string{a.ID}}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("image-only message: %d", resp.StatusCode)
	}
	var sent struct {
		ID          string             `json:"id"`
		Attachments []store.Attachment `json:"attachments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sent); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(sent.Attachments) != 1 || sent.Attachments[0].ID != a.ID {
		t.Fatalf("sent attachments: %+v", sent.Attachments)
	}
	resp = f.request(http.MethodGet, a.URL, nil, false)
	if resp.StatusCode != 200 {
		t.Fatalf("read: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]any{"body": "reuse", "attachments": []string{a.ID}}, true)
	if resp.StatusCode != 400 {
		t.Fatalf("reuse: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages", nil, false)
	var messages []struct {
		Attachments []store.Attachment `json:"attachments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, m := range messages {
		if len(m.Attachments) == 1 && m.Attachments[0].ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("attachment missing after reload")
	}
	// A project member outside a DM cannot fetch even a staged blob in that DM.
	dm, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "dm", DMKey: "test-private", Title: "private", CreatedBy: "peer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SaveAttachment(t.Context(), dm.ID, "owner", "private.png", bytes.NewReader(testPNG)); err == nil {
		t.Fatal("nonmember uploaded to DM")
	}
	_, _, path, err := f.db.AttachmentByID(t.Context(), a.ID, f.ownerUserID(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO attachment(id,project_id,room_id,sha256,mime,size,path,created_by,created_at) VALUES('att_private',?,?,?,?,?,?,?,?)`, f.project.ID, dm.ID, "hash", "image/png", len(testPNG), path, "peer", "now")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// An attachment the person may not open answers like one that does not exist, in status and in body.
	resp = f.request(http.MethodGet, "/api/attachments/att_private", nil, false)
	private, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	missing := f.request(http.MethodGet, "/api/attachments/att_does_not_exist", nil, false)
	unknown, _ := io.ReadAll(missing.Body)
	missing.Body.Close()
	if resp.StatusCode != 404 || missing.StatusCode != 404 || string(private) != string(unknown) {
		t.Fatalf("private image read: %d %q, a missing one: %d %q", resp.StatusCode, private, missing.StatusCode, unknown)
	}
}

func TestAttachmentTypeAndSize(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	for _, data := range [][]byte{{}, bytes.Repeat([]byte("x"), (10<<20)+1)} {
		resp, _ := uploadImage(t, f, f.room.ID, data)
		if resp.StatusCode != 400 && resp.StatusCode != 413 {
			t.Fatalf("invalid upload: %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp, file := uploadFile(t, f, f.room.ID, "notes.txt", []byte("not an image"))
	if resp.StatusCode != 201 || file.Name != "notes.txt" || file.MIME != "text/plain; charset=utf-8" {
		t.Fatalf("arbitrary file upload: %d %+v", resp.StatusCode, file)
	}
	resp = f.request(http.MethodGet, file.URL, nil, false)
	if resp.StatusCode != 200 || resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Disposition") != `attachment; filename=notes.txt` {
		t.Fatalf("file download: %d headers=%v", resp.StatusCode, resp.Header)
	}
	resp.Body.Close()
	resp, a := uploadImage(t, f, f.room.ID, testPNG)
	if resp.StatusCode != 201 {
		t.Fatalf("valid upload: %d", resp.StatusCode)
	}
	_, _, path, err := f.db.AttachmentByID(t.Context(), a.ID, f.ownerUserID(t))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v", info.Mode())
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE attachment SET created_at='2000-01-01T00:00:00.000000000Z' WHERE id=?`, a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SweepStagedAttachments(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired upload remains: %v", err)
	}
}

func TestAttachmentDownloadHeaders(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	// An SVG with script in it is not classified as an image: it keeps its sniffed text type and stays a download.
	resp, xml := uploadFile(t, f, f.room.ID, "drawing.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("SVG upload: %d", resp.StatusCode)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE attachment SET filename='' WHERE id=?`, xml.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodGet, xml.URL, nil, false)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Disposition") != `attachment; filename=attachment` || resp.Header.Get("Content-Security-Policy") != "sandbox; default-src 'none'" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("empty-name SVG download: %d headers=%v", resp.StatusCode, resp.Header)
	}
	resp.Body.Close()
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE attachment SET mime='image/svg+xml' WHERE id=?`, xml.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodGet, xml.URL, nil, false)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Disposition") != `attachment; filename=attachment` {
		t.Fatalf("SVG MIME download: %d headers=%v", resp.StatusCode, resp.Header)
	}
	resp.Body.Close()

	resp, png := uploadImage(t, f, f.room.ID, testPNG)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PNG upload: %d", resp.StatusCode)
	}
	resp = f.request(http.MethodGet, png.URL, nil, false)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Disposition") != "" || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
		t.Fatalf("inline PNG: %d headers=%v", resp.StatusCode, resp.Header)
	}
	resp.Body.Close()
}

func TestAgentAttachmentFetchRequiresDeliveredTurnAndMembership(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, delivery := f.execution(t)
	resp, a := uploadImage(t, f, f.room.ID, testPNG)
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE attachment SET message_id=? WHERE id=?`, delivery.Notes[0].MessageID, a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	key, err := f.srv.Auth.IssueAgentKey(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	keyHash, _, err := f.db.AgentKeyByID(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/agent/turns/" + turn.ID + "/attachments/" + a.ID
	request := func(bearer, url string) int {
		req, err := http.NewRequest(http.MethodGet, f.http.URL+url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if status := request(attachmentBearer(keyHash, turn.ID), path); status != 200 {
		t.Fatalf("authorized fetch: %d", status)
	}
	if status := request("wrong", path); status != 404 {
		t.Fatalf("invalid key: %d", status)
	}
	if status := request(attachmentBearer(keyHash, turn.ID), "/api/agent/turns/turn_other/attachments/"+a.ID); status != 404 {
		t.Fatalf("unrelated turn: %d", status)
	}
	if status := request(f.machineKey, "/api/machine/turns/"+turn.ID+"/attachments/"+a.ID); status != 404 {
		t.Fatalf("old machine route still exposed: %d", status)
	}
	toolbox := agenttools.Server{URL: f.http.URL, Key: key}
	response, notification := toolbox.Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attachment_get","arguments":{"attachment_id":"`+a.ID+`"}}}`))
	if notification || !bytes.Contains(response, []byte(`"type":"image"`)) {
		t.Fatalf("MCP attachment read: %s", response)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE room_member SET left_at='2026-10-14T00:00:00Z' WHERE room_id=? AND member_kind='agent' AND member_id=?`, f.room.ID, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status := request(attachmentBearer(keyHash, turn.ID), path); status != 404 {
		t.Fatalf("former member fetch: %d", status)
	}
	response, _ = toolbox.Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"attachment_get","arguments":{"attachment_id":"`+a.ID+`"}}}`))
	if !bytes.Contains(response, []byte(`"isError":true`)) {
		t.Fatalf("former member MCP fetch: %s", response)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE room_member SET left_at=NULL WHERE room_id=? AND member_kind='agent' AND member_id=?`, f.room.ID, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, turn.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status := request(attachmentBearer(keyHash, turn.ID), path); status != request("wrong", path) || status != 404 {
		t.Fatalf("terminal turn fetch: %d", status)
	}
	response, _ = toolbox.Process(t.Context(), []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"attachment_get","arguments":{"attachment_id":"`+a.ID+`"}}}`))
	if !bytes.Contains(response, []byte(`"isError":true`)) {
		t.Fatalf("terminal turn MCP fetch: %s", response)
	}
}

// An uploaded image reports its pixel size, so a thumbnail can hold its place; a file that is not an image has none.
func TestUploadedImageReportsItsSize(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp, png := uploadImage(t, f, f.room.ID, testPNG)
	if resp.StatusCode != http.StatusCreated || png.Width < 1 || png.Height < 1 {
		t.Fatalf("PNG upload: %d %+v", resp.StatusCode, png)
	}
	resp, text := uploadFile(t, f, f.room.ID, "note.txt", []byte("plain words"))
	if resp.StatusCode != http.StatusCreated || text.Width != 0 || text.Height != 0 {
		t.Fatalf("text upload: %d %+v", resp.StatusCode, text)
	}
}

func TestSVGAttachmentIsAnImageOnlyForImageRequests(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	const plain = `<svg xmlns="http://www.w3.org/2000/svg" width="40" height="20"><rect width="40" height="20"/></svg>`
	resp, svg := uploadFile(t, f, f.room.ID, "drawing.svg", []byte(plain))
	if resp.StatusCode != http.StatusCreated || svg.MIME != "image/svg+xml" || svg.Width != 40 || svg.Height != 20 {
		t.Fatalf("a plain SVG upload: %d %+v", resp.StatusCode, svg)
	}
	get := func(dest string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, f.http.URL+svg.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(f.cookie)
		if dest != "" {
			req.Header.Set("Sec-Fetch-Dest", dest)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// Loaded by an <img>: inline, with the strict headers.
	resp = get("image")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/svg+xml" || resp.Header.Get("Content-Disposition") != "" || string(body) != plain {
		t.Fatalf("image request: %d %v", resp.StatusCode, resp.Header)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "sandbox; default-src 'none'; style-src 'unsafe-inline'" || resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("SVG headers: %v", resp.Header)
	}
	// Any other request is a download: a tab opened on the address, a script fetch, a client that sends nothing.
	for _, dest := range []string{"document", "iframe", "embed", "object", "script", "empty", ""} {
		resp = get(dest)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("Sec-Fetch-Dest %q was not a download: %v", dest, resp.Header)
		}
	}
	// An SVG with active content is stored as it was before: text, download only.
	for name, active := range map[string]string{
		"script":  `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"handler": `<svg xmlns="http://www.w3.org/2000/svg" onload="x()"/>`,
		"link":    `<svg xmlns="http://www.w3.org/2000/svg"><a href="https://example.test/"><rect/></a></svg>`,
		"doctype": `<!DOCTYPE svg [<!ENTITY a "b">]><svg xmlns="http://www.w3.org/2000/svg">&a;</svg>`,
	} {
		resp, att := uploadFile(t, f, f.room.ID, name+".svg", []byte(active))
		if resp.StatusCode != http.StatusCreated || att.MIME == "image/svg+xml" || !strings.HasPrefix(att.MIME, "text/") {
			t.Fatalf("%s: %d %+v", name, resp.StatusCode, att)
		}
		req, _ := http.NewRequest(http.MethodGet, f.http.URL+att.URL, nil)
		req.AddCookie(f.cookie)
		req.Header.Set("Sec-Fetch-Dest", "image")
		got, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got.Body.Close()
		if !strings.HasPrefix(got.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("%s was shown inline: %v", name, got.Header)
		}
	}
}

// An upload no message has claimed is a draft: only the person who made it can open it, and they can only have so many at once.
func TestStagedAttachmentsAreTheUploadersAndCapped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp, staged := uploadImage(t, f, f.room.ID, testPNG)
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	cookie, csrf := f.projectMember(t, "otherreader", f.project.ID, "operator")
	get := func(c *http.Cookie, token string) int {
		r := f.requestAs(t, c, token, "GET", staged.URL, nil)
		defer r.Body.Close()
		return r.StatusCode
	}
	if got := get(cookie, csrf); got != http.StatusNotFound {
		t.Fatalf("another member opened someone's unsent upload: %d", got)
	}
	if got := get(f.cookie, f.csrf); got != http.StatusOK {
		t.Fatalf("the uploader cannot open their own draft: %d", got)
	}
	// The rest of the cap, then one more.
	for i := 1; i < store.MaxStagedAttachments; i++ {
		if r, _ := uploadImage(t, f, f.room.ID, testPNG); r.StatusCode != 201 {
			t.Fatalf("upload %d within the cap: %d", i+1, r.StatusCode)
		}
	}
	if r, _ := uploadImage(t, f, f.room.ID, testPNG); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("an upload past the cap: %d", r.StatusCode)
	}
	// Removing one makes room again, and the cap is per person.
	del := f.requestAs(t, f.cookie, f.csrf, "DELETE", "/api/rooms/"+f.room.ID+"/attachments/"+staged.ID, nil)
	del.Body.Close()
	if r, _ := uploadImage(t, f, f.room.ID, testPNG); r.StatusCode != 201 {
		t.Fatalf("an upload after one was removed: %d", r.StatusCode)
	}
}
