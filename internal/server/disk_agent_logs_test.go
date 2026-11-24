package server

import (
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/agentlog"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestDiskReportCountsRemoteMachinesAgentLogs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	create := func(name string, approved bool) store.Machine {
		var machine store.Machine
		if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
			var err error
			machine, err = tx.CreateMachine(store.Machine{Name: name, KeyHash: "key-" + name, Approved: approved})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return machine
	}
	online, old := create("laptop", true), create("old-box", true)
	create("offline-box", true)
	create("unapproved-box", false)
	connect := func(id string, size bool) *machineConn {
		conn := &machineConn{id: id, supportsAgentLogSize: size, sendQueue: make(chan protocol.Frame, 4), done: make(chan struct{})}
		f.srv.machineMu.Lock()
		f.srv.machines[id] = conn
		f.srv.machineMu.Unlock()
		return conn
	}
	laptop := connect(online.ID, true)
	oldBox := connect(old.ID, false)
	go func() {
		request := <-laptop.sendQueue
		if request.Type != protocol.AgentLogUsage || request.AgentID != "" {
			close(laptop.done)
			return
		}
		details, _ := json.Marshal(agentlog.Size{Bytes: 5000, Agents: 2})
		laptop.harnessMu.Lock()
		waiter := laptop.pathPending[request.ReqID]
		laptop.harnessMu.Unlock()
		waiter <- protocol.Frame{V: protocol.Version, Type: protocol.AgentLogUsageResult, ReqID: request.ReqID, Details: details}
	}()

	report, err := f.srv.buildDiskReport(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(oldBox.sendQueue) != 0 {
		t.Fatal("a supervisor that does not report agent output received the request")
	}
	rows := map[string]diskMachine{}
	for _, m := range report.Machines {
		rows[m.Name] = m
	}
	if len(rows) != 3 {
		t.Fatalf("want the three approved remote machines, got %+v", report.Machines)
	}
	if got := rows["laptop"]; got.LogBytes != 5000 || got.LogAgents != 2 || got.Error != "" {
		t.Errorf("laptop: %+v", got)
	}
	if got := rows["old-box"]; got.LogBytes != 0 || got.Error == "" {
		t.Errorf("old-box: %+v", got)
	}
	if got := rows["offline-box"]; got.LogBytes != 0 || got.Error != "machine is offline" {
		t.Errorf("offline-box: %+v", got)
	}
	var remote, sum int64 = -1, 0
	for _, c := range report.Categories {
		if c.Key == "remote_agent_logs" {
			remote = c.Bytes
		}
		sum += c.Bytes
	}
	if remote != 5000 || report.TotalBytes != sum {
		t.Errorf("remote_agent_logs=%d total=%d sum of categories=%d", remote, report.TotalBytes, sum)
	}
}

func TestDiskReportDoesNotWaitLongForASilentMachine(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var machine store.Machine
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		machine, err = tx.CreateMachine(store.Machine{Name: "silent-box", KeyHash: "key-silent", Approved: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	conn := &machineConn{id: machine.ID, supportsAgentLogSize: true, sendQueue: make(chan protocol.Frame, 4), done: make(chan struct{})}
	f.srv.machineMu.Lock()
	f.srv.machines[machine.ID] = conn
	f.srv.machineMu.Unlock()
	start := time.Now()
	report, err := f.srv.buildDiskReport(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < sizeWait || took > sizeWait+15*time.Second {
		t.Errorf("the report took %v; a silent machine should cost about %v", took, sizeWait)
	}
	if len(report.Machines) != 1 || report.Machines[0].Error == "" || report.Machines[0].LogBytes != 0 {
		t.Errorf("silent machine row: %+v", report.Machines)
	}
}
