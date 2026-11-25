package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

type instancesAnswer struct {
	Instances []instanceRow `json:"instances"`
}

type goneAnswer struct {
	JournalID     string `json:"journal_id"`
	AlreadyGone   bool   `json:"already_gone"`
	TurnsLost     int    `json:"turns_lost"`
	CommandsEnded int    `json:"commands_ended"`
}

// remoteWithLostInstance makes the fixture's machine a remote one whose instance "journal-lost" owns the agent's open turn and one running command.
func remoteWithLostInstance(t *testing.T, f *fixture) (agent store.Agent, turn store.Turn) {
	t.Helper()
	agent, turn, note, _ := f.execution(t)
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE machine SET is_host=0 WHERE id=?`, f.machineID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordSupervisorInstance(ctx, f.machineID, "journal-lost", "build-1", "native"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}, JournalID: "journal-lost"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	job, _, err := f.db.CreateCommandJob(ctx, store.CommandJob{AgentID: agent.ID, MachineID: f.machineID, JournalID: "journal-lost", ProjectID: f.project.ID, RoomID: f.room.ID, Cmd: "sleep 1", CheckoutPath: "/tmp/checkout"},
		store.RequestKey{ActorKind: "agent", ActorID: agent.ID, Kind: "command_run", Key: "k", Digest: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordCommandStarted(ctx, job.ID, job.JournalID, "2026-11-25T00:00:00.000000000Z", "/v/log", "/v"); err != nil {
		t.Fatal(err)
	}
	return agent, turn
}

func TestWritingOffARemoteInstanceByHand(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn := remoteWithLostInstance(t, f)
	base := "/api/machines/" + f.machineID + "/instances"
	gone := func(journal string) (int, goneAnswer) {
		resp := f.request("POST", base+"/"+journal+"/gone", nil, true)
		defer resp.Body.Close()
		var out goneAnswer
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Someone who is neither the owner nor an admin sees and loses nothing.
	cookie, csrf := registerSecondUser(t, f, "outsider")
	for _, call := range [][2]string{{"GET", base}, {"POST", base + "/journal-lost/gone"}} {
		resp := f.requestAs(t, cookie, csrf, call[0], call[1], nil)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("%s %s as outsider: %d", call[0], call[1], resp.StatusCode)
		}
	}
	if owner := turnOpen(t, f, agent.ID); owner != "journal-lost" {
		t.Fatalf("an outsider's request ended the turn: %q", owner)
	}

	// The listing says what a write-off would lose.
	resp := f.request("GET", base, nil, false)
	var listed instancesAnswer
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(listed.Instances) != 1 {
		t.Fatalf("listing %d %+v", resp.StatusCode, listed)
	}
	got := listed.Instances[0]
	if got.JournalID != "journal-lost" || got.State != "active" || got.Connected || got.Turns != 1 || got.Commands != 1 || len(got.Agents) != 1 || got.Agents[0].ID != agent.ID || got.Agents[0].Slug != "worker" {
		t.Fatalf("instance row %+v", got)
	}

	// A connected instance is alive: refused, and nothing is lost.
	conn := &machineConn{id: f.machineID, journalID: "journal-lost", sendQueue: make(chan protocol.Frame, 4), done: make(chan struct{}), bound: map[string]int{}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = conn
	f.srv.machineMu.Unlock()
	if status, _ := gone("journal-lost"); status != 409 {
		t.Fatalf("connected instance: %d", status)
	}
	resp = f.request("GET", base, nil, false)
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if !listed.Instances[0].Connected || turnOpen(t, f, agent.ID) != "journal-lost" {
		t.Fatalf("connected instance lost work: %+v", listed.Instances[0])
	}
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID)
	f.srv.machineMu.Unlock()

	ageInstance(t, f, "journal-lost")
	if status, _ := gone("journal-unknown"); status != 404 {
		t.Fatalf("unknown instance: %d", status)
	}

	// Now it is written off: the turn is lost with the recovery notice and the agent waits for recovery, the command ends, the person is recorded.
	status, out := gone("journal-lost")
	if status != 200 || out.AlreadyGone || out.TurnsLost != 1 || out.CommandsEnded != 1 {
		t.Fatalf("write-off %d %+v", status, out)
	}
	if owner := turnOpen(t, f, agent.ID); owner != "" {
		t.Fatalf("the turn is still open on %q", owner)
	}
	ctx := context.Background()
	owner, err := f.db.UserByLogin(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.db.EventsAfter(ctx, owner.ID, 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	notices, records := 0, []store.Event{}
	for _, event := range events {
		switch event.Kind {
		case "supervisor_journal_changed":
			if event.TurnID == turn.ID {
				notices++
			}
		case "supervisor_instance_gone":
			records = append(records, event)
		}
	}
	if notices != 1 || len(records) != 1 {
		t.Fatalf("recovery notices %d, write-off records %d", notices, len(records))
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(records[0].PayloadJSON), &payload); err != nil || payload["actor_id"] != owner.ID || payload["journal_id"] != "journal-lost" || payload["machine_id"] != f.machineID {
		t.Fatalf("record payload %v %v", payload, err)
	}
	if runtime, err := f.db.AgentRuntimeByID(ctx, agent.ID); err != nil || !runtime.RecoveryBlock {
		t.Fatalf("the agent must wait for recovery: %+v %v", runtime, err)
	}

	// A second call does nothing and says so; the listing shows it gone and owning nothing.
	status, out = gone("journal-lost")
	if status != 200 || !out.AlreadyGone || out.TurnsLost != 0 || out.CommandsEnded != 0 {
		t.Fatalf("second write-off %d %+v", status, out)
	}
	events, _ = f.db.EventsAfter(ctx, owner.ID, 0, 1<<62)
	again := 0
	for _, event := range events {
		if event.Kind == "supervisor_instance_gone" {
			again++
		}
	}
	if again != 1 {
		t.Fatalf("a second call recorded another decision: %d", again)
	}
	resp = f.request("GET", base, nil, false)
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if g := listed.Instances[0]; g.State != "gone" || g.GoneAt == "" || g.Turns != 0 || g.Commands != 0 || len(g.Agents) != 0 {
		t.Fatalf("listing after %+v", g)
	}
}

// An instance that said hello within the last minute is refused (409 instance_recently_seen) even when this server holds no connection for it
// (the hello landed after the connected check, or the socket just closed), and loses nothing; once it has been silent it is written off.
func TestWritingOffARecentlySeenInstanceIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, _ := remoteWithLostInstance(t, f)
	path := "/api/machines/" + f.machineID + "/instances/journal-lost/gone"
	post := func() (int, string) {
		resp := f.request("POST", path, nil, true)
		defer resp.Body.Close()
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Error
	}
	if status, code := post(); status != 409 || code != "instance_recently_seen" {
		t.Fatalf("recently seen: %d %q", status, code)
	}
	if owner := turnOpen(t, f, agent.ID); owner != "journal-lost" {
		t.Fatalf("a refused write-off lost the turn: %q", owner)
	}
	ageInstance(t, f, "journal-lost")
	if status, _ := post(); status != 200 {
		t.Fatalf("after the quiet period: %d", status)
	}
	if owner := turnOpen(t, f, agent.ID); owner != "" {
		t.Fatalf("the turn is still open on %q", owner)
	}
}

// An instance connected long ago whose connection ends just now was seen until it dropped: the write-off is refused with
// instance_recently_seen (the quiet check counts a disconnect as a sighting, F005/F006), and goes through after the quiet window.
// An instance connected long ago whose connection ends just now was seen until it dropped: the write-off is refused with
// instance_recently_seen (the quiet check counts a disconnect as a sighting, F005/F006), and goes through after the quiet window.
func TestWritingOffAnInstanceThatJustDisconnected(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "matching"
	agent, turn := remoteWithLostInstance(t, f)
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE machine SET is_host=0 WHERE id=?`, f.machineID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/machines/" + f.machineID + "/instances/journal-lost/gone"
	post := func() (int, string) {
		resp := f.request("POST", path, nil, true)
		defer resp.Body.Close()
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Error
	}
	// The hello vouches for the open turn it owns, as a live instance would; the connection ran for hours until it drops now.
	conn, _ := dialInstance(t, f, "journal-lost", "matching", nil, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0,
		OpenTurns: []protocol.OpenTurn{{TurnID: turn.ID, Exec: "running", ProcessAlive: true}}}})
	// The hello is old; the instance was connected until now.
	ageInstance(t, f, "journal-lost")
	conn.CloseNow()
	waitForGuard(t, 3*time.Second, func() bool {
		f.srv.machineMu.Lock()
		defer f.srv.machineMu.Unlock()
		return f.srv.machines[f.machineID] == nil
	})
	if status, code := post(); status != 409 || code != "instance_recently_seen" {
		t.Fatalf("just disconnected: %d %q", status, code)
	}
	if owner := turnOpen(t, f, agent.ID); owner != "journal-lost" {
		t.Fatalf("a refused write-off lost the turn: %q", owner)
	}
	ageInstance(t, f, "journal-lost")
	if status, _ := post(); status != 200 {
		t.Fatalf("after the quiet period: %d", status)
	}
}

// ageInstance makes the instance's last hello old enough for a person to write it off.
func ageInstance(t *testing.T, f *fixture, journalID string) {
	t.Helper()
	old := time.Now().UTC().Add(-2 * store.InstanceQuietFor).Format("2006-01-02T15:04:05.000000000Z")
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE supervisor_instance SET last_seen_at=? WHERE machine_id=? AND journal_id=?`, old, f.machineID, journalID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A revoked machine's instances are still listed and written off (F007): the machine will never reconnect, so its open work is exactly the
// case a write-off exists for, and the routes do not gate on revocation.
func TestInstanceRoutesServeARevokedMachine(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	remoteWithLostInstance(t, f)
	if _, err := f.exec(`UPDATE machine SET revoked_at='2026-10-24T00:00:00Z' WHERE id=?`, f.machineID); err != nil {
		t.Fatal(err)
	}
	base := "/api/machines/" + f.machineID + "/instances"
	resp := f.request("GET", base, nil, false)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("listing on a revoked machine: %d", resp.StatusCode)
	}
	var listed instancesAnswer
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	if len(listed.Instances) != 1 || listed.Instances[0].JournalID != "journal-lost" || listed.Instances[0].Turns != 1 {
		t.Fatalf("listing on a revoked machine: %+v", listed)
	}
	ageInstance(t, f, "journal-lost")
	resp = f.request("POST", base+"/journal-lost/gone", nil, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("write-off on a revoked machine: %d", resp.StatusCode)
	}
}

func TestInstanceRoutesRefuseTheHostMachine(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, call := range [][2]string{{"GET", "/api/machines/" + f.machineID + "/instances"}, {"POST", "/api/machines/" + f.machineID + "/instances/j/gone"}} {
		resp := f.request(call[0], call[1], nil, call[0] == "POST")
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s %s on the host: %d", call[0], call[1], resp.StatusCode)
		}
	}
	resp := f.request("GET", "/api/machines/mch_nope/instances", nil, false)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown machine: %d", resp.StatusCode)
	}
}

// turnOpen says whether the agent still has an open turn on the instance: "journal-lost" while it runs, "" once it ended or was lost.
func turnOpen(t *testing.T, f *fixture, agentID string) string {
	t.Helper()
	owner, err := f.db.AgentOpenTurnOwner(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}
