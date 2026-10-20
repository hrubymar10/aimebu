package server

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/auth"
	"github.com/hrubymar10/aimebu/internal/store"
)

// The account an import makes has a password nobody knows; after the import commits the server makes a one-time reset link for it,
// and following the link is the only way in.
func TestImportMakesOneTimeInviteLinksForNewAccounts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	var ownerID string
	{
		response := f.request(http.MethodGet, "/api/me", nil, false)
		var me struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(response.Body).Decode(&me); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		ownerID = me.ID
	}
	hash, params, err := auth.HashPassword("a secret nobody here knows", f.srv.Auth.Policy)
	if err != nil {
		t.Fatal(err)
	}
	var newbie store.User
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		var err error
		newbie, err = tx.CreateUser(store.User{Login: "newbie", PWHash: hash, PWParams: params})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	signIn := func(password string) int {
		body, _ := json.Marshal(map[string]string{"login": "newbie", "password": password})
		response, err := http.Post(f.http.URL+"/api/login", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if signIn("a brand new password!") == 200 {
		t.Fatal("the account cannot be signed into before the link is used")
	}

	f.srv.importMu.Lock()
	f.srv.importJob = importV1JobState{ID: "job1", Phase: "commit"}
	f.srv.importMu.Unlock()
	f.srv.createImportInvites(ctx, "job1", ownerID, "http://example.test", []store.V1NewAccount{{V1Name: "bob", Login: "newbie", UserID: newbie.ID, Role: "viewer"}})
	f.srv.importMu.Lock()
	job := f.srv.importJob
	f.srv.importMu.Unlock()
	if job.InviteError != "" || len(job.Invites) != 1 || job.Invites[0].Login != "newbie" || job.Invites[0].ExpiresAt == "" || !strings.HasPrefix(job.Invites[0].URL, "http://example.test/#invite=aui_") {
		t.Fatalf("the job carries one invite for the new account: %+v", job)
	}
	token, err := url.QueryUnescape(strings.TrimPrefix(job.Invites[0].URL, "http://example.test/#invite="))
	if err != nil {
		t.Fatal(err)
	}
	redeem := func() int {
		body, _ := json.Marshal(map[string]string{"token": token, "password": "a brand new password!"})
		response, err := http.Post(f.http.URL+"/api/invites/redeem", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if code := redeem(); code != 201 {
		t.Fatalf("redeeming the link: %d", code)
	}
	if signIn("a brand new password!") != 200 {
		t.Fatal("the person can sign in with the password they chose")
	}
	if code := redeem(); code == 201 {
		t.Fatal("the link works once")
	}
	// An invite that cannot be made is reported, not hidden, and no link is invented.
	f.srv.importMu.Lock()
	f.srv.importJob = importV1JobState{ID: "job2"}
	f.srv.importMu.Unlock()
	f.srv.createImportInvites(ctx, "job2", ownerID, "http://example.test", []store.V1NewAccount{{Login: "ghost", UserID: "usr_missing"}})
	f.srv.importMu.Lock()
	failed := f.srv.importJob
	f.srv.importMu.Unlock()
	if failed.InviteError == "" || len(failed.Invites) != 0 {
		t.Fatalf("a failed invite is reported: %+v", failed)
	}
}
