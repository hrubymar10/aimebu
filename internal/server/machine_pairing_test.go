package server

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/auth"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestMachinePairingApprovalRevocationAndProjectFolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp := f.request("POST", "/api/machine-pairings", map[string]string{"name": "laptop"}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("create pairing: %d", resp.StatusCode)
	}
	var pairing struct{ Token, Command string }
	if err := json.NewDecoder(resp.Body).Decode(&pairing); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if pairing.Token == "" || pairing.Command == "" {
		t.Fatalf("empty pairing: %+v", pairing)
	}
	resp = f.request("POST", "/api/machine-pairings/redeem", map[string]string{"token": pairing.Token}, false)
	if resp.StatusCode != 201 {
		t.Fatalf("redeem: %d", resp.StatusCode)
	}
	var credential struct {
		MachineID  string `json:"machine_id"`
		MachineKey string `json:"machine_key"`
		Approved   bool   `json:"approved"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&credential); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if credential.MachineID == "" || credential.MachineKey == "" || credential.Approved {
		t.Fatalf("pending credential: %+v", credential)
	}
	resp = f.request("POST", "/api/machine-pairings/redeem", map[string]string{"token": pairing.Token}, false)
	if resp.StatusCode != 401 {
		t.Fatalf("reused token: %d", resp.StatusCode)
	}
	resp.Body.Close()
	status := func() int {
		req, _ := http.NewRequest("GET", f.http.URL+"/api/machines/"+credential.MachineID+"/pair-status", nil)
		req.Header.Set("Authorization", "Bearer "+credential.MachineKey)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if got := status(); got != 200 {
		t.Fatalf("pending status: %d", got)
	}
	resp = f.request("GET", "/api/machines", nil, false)
	var list struct {
		Machines []machineRow `json:"machines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list.Machines) != 2 || !list.Machines[0].IsHost || list.Machines[1].Approved {
		t.Fatalf("machine list: %+v", list.Machines)
	}
	resp = f.request("PUT", "/api/projects/"+f.project.ID+"/machine-folders/"+credential.MachineID, map[string]string{"folder": "/work/project"}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("folder mapping: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("POST", "/api/machines/"+credential.MachineID+"/approve", nil, true)
	if resp.StatusCode != 200 {
		t.Fatalf("approve: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("POST", "/api/machines/"+credential.MachineID+"/revoke", nil, true)
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := status(); got != 401 {
		t.Fatalf("revoked key status: %d", got)
	}
}

func TestMachinePairingCommandUsesPublicURL(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	command := func() string {
		t.Helper()
		resp := f.request("POST", "/api/machine-pairings", map[string]string{"name": "laptop"}, true)
		defer resp.Body.Close()
		var pairing struct{ Token, Command string }
		if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&pairing) != nil {
			t.Fatalf("create pairing: %d", resp.StatusCode)
		}
		return pairing.Command
	}
	if got, want := command(), "aimebu supervisor --pair '"+f.http.URL+"' 'amp_"; !strings.HasPrefix(got, want) {
		t.Fatalf("direct command=%q, want prefix %q", got, want)
	}
	f.srv.Auth.PublicURL = "https://aimebu.example.com"
	if got, want := command(), "aimebu supervisor --pair 'https://aimebu.example.com' 'amp_"; !strings.HasPrefix(got, want) {
		t.Fatalf("proxied command=%q, want prefix %q", got, want)
	}
}

func TestProjectMemberMachineListExcludesPendingAndRevoked(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	hash, params, err := auth.HashPassword("operator password", f.srv.Auth.Policy)
	if err != nil {
		t.Fatal(err)
	}
	var approvedID string
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		user, err := tx.CreateUser(store.User{Login: "operator", PWHash: hash, PWParams: params})
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'operator')`, f.project.ID, user.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.CreateMachine(store.Machine{Name: "pending", KeyHash: "hash"})
		if err != nil {
			return err
		}
		approved, err := tx.CreateMachine(store.Machine{Name: "approved", KeyHash: "hash", Approved: true})
		approvedID = approved.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	login := f.request("POST", "/api/login", map[string]string{"login": "operator", "password": "operator password"}, false)
	if login.StatusCode != 200 {
		t.Fatalf("operator login: %d", login.StatusCode)
	}
	operatorCookie := login.Cookies()[0]
	login.Body.Close()
	request := func(path string) (int, []machineRow) {
		t.Helper()
		req, _ := http.NewRequest("GET", f.http.URL+path, nil)
		req.AddCookie(operatorCookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Machines []machineRow `json:"machines"`
		}
		if resp.StatusCode == 200 {
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, body.Machines
	}
	if status, _ := request("/api/machines"); status != 403 {
		t.Fatalf("unscoped list status = %d", status)
	}
	if status, _ := request("/api/machines?project_id=prj_missing"); status != 403 {
		t.Fatalf("other project list status = %d", status)
	}
	status, machines := request("/api/machines?project_id=" + f.project.ID)
	if status != 200 || len(machines) != 2 || !machines[0].IsHost || machines[1].Name != "approved" {
		t.Fatalf("project machine list status=%d machines=%+v", status, machines)
	}
	ownerPicker := f.request("GET", "/api/machines?project_id="+f.project.ID, nil, false)
	var ownerChoices struct {
		Machines []machineRow `json:"machines"`
	}
	if err := json.NewDecoder(ownerPicker.Body).Decode(&ownerChoices); err != nil {
		t.Fatal(err)
	}
	ownerPicker.Body.Close()
	if ownerPicker.StatusCode != 200 || len(ownerChoices.Machines) != 2 {
		t.Fatalf("owner picker status=%d machines=%+v", ownerPicker.StatusCode, ownerChoices.Machines)
	}
	if err := f.db.RevokeMachine(context.Background(), approvedID); err != nil {
		t.Fatal(err)
	}
	status, machines = request("/api/machines?project_id=" + f.project.ID)
	if status != 200 || len(machines) != 1 || !machines[0].IsHost {
		t.Fatalf("list after revoke status=%d machines=%+v", status, machines)
	}
}

// The Spawn dialog offers a worktree only where the server would make one: the host machine row carries the server's own check and the
// reason it gives when the folder cannot have one.
func TestHostMachineRowSaysWhetherTheProjectFolderCanHaveAWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := newFixture(t)
	f.seed()
	check := func() *worktreeCheck {
		t.Helper()
		resp := f.request("GET", "/api/machines?project_id="+f.project.ID, nil, false)
		var body struct {
			Machines []machineRow `json:"machines"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, machine := range body.Machines {
			if machine.IsHost {
				return machine.ProjectWorktree
			}
		}
		t.Fatal("no host machine in the list")
		return nil
	}
	if got := check(); got == nil || got.OK || got.Code != "not_git_repo" || got.Reason == "" {
		t.Fatalf("a plain folder: %+v", got)
	}
	if out, err := exec.Command("git", "-C", f.project.Folder, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if got := check(); got == nil || !got.OK {
		t.Fatalf("a git repository: %+v", got)
	}
	// A folder inside a repository is not its root: the server refuses it at spawn, so the dialog must not offer it either.
	nested := filepath.Join(f.project.Folder, "sub")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`UPDATE project SET folder=? WHERE id=?`, nested, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if got := check(); got == nil || got.OK || got.Code != "not_git_root" {
		t.Fatalf("a folder inside a repository: %+v", got)
	}
}
