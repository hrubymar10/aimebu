package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

// A machine runs several supervisor instances side by side while an older build finishes its work (docs/supervisor.md, "Side by side"). The
// server decides what each one is (it routes new work to the active one and tells the others to retire); the manager here keeps them alive:
// it starts the next instance on a journal of its own when none is active, finishes the journal of a dead one, and cuts a retiring instance
// that has stopped being alive. It never cuts one that is doing work.
const (
	instancePassEvery = 2 * time.Second
	// instanceStartWait is how long an instance that holds its journal without a server connection may be the only hope for an active
	// instance before another is started next to it. A healthy supervisor connects within seconds, also after a server restart.
	instanceStartWait = 30 * time.Second
	// instanceSpawnGuard keeps the manager from starting a second process before the first has taken its journal lock.
	instanceSpawnGuard = 15 * time.Second
	// instanceInspectEvery is how often a journal whose lock is free is opened to see what it owes.
	instanceInspectEvery = 5 * time.Second
	// maxUnconnectedInstances stops a process that never connects from being answered with ever more of them.
	maxUnconnectedInstances = 3
	// instanceVanishedAfter is how long an instance the server knows must have been gone from the machine before it is written off.
	instanceVanishedAfter = time.Minute
	instanceHandoverAgain = 5 * time.Minute
)

// A retiring instance is cut only when it is not alive: it holds its lock without a server connection for retireDisconnectLimit (a healthy one
// reconnects every five seconds), or the server sees nothing it runs for retireEmptyLimit and it still has not exited. Variables so tests shrink them.
var (
	retireDisconnectLimit = 10 * time.Minute
	retireEmptyLimit      = time.Minute
)

// instanceView is one journal of the machine and who holds it.
type instanceView struct {
	path      string
	held      bool
	pid       int    // the verified holder, 0 when the lock is held by a process that cannot be verified as a supervisor
	journalID string // "" for a holder from before instances wrote their ID in the lock file, and for a free journal not yet opened
}

// instanceTrack is what the manager remembers of one journal between passes.
type instanceTrack struct {
	heldSince, unconnectedSince, emptySince time.Time
	inspectedAt, spawnedAt, handoverAt      time.Time
	journalID                               string // learned from the lock, or by opening the journal while it was free
}

func (m *detachedSupervisorManager) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

func (m *detachedSupervisorManager) supervisorDir() string {
	return filepath.Join(m.config.root, "supervisor")
}

// scanInstances lists the journals that exist and who holds each.
func (m *detachedSupervisorManager) scanInstances() []instanceView {
	if m.scan != nil {
		return m.scan()
	}
	var views []instanceView
	for _, path := range supervisor.InstanceJournals(m.supervisorDir()) {
		held, _, id, err := supervisor.JournalLockInfo(path)
		if err != nil {
			continue
		}
		view := instanceView{path: path, held: held, journalID: id}
		if held {
			if pid, ok, err := journalHolderPID(path); err == nil && ok {
				view.pid = pid
			}
		}
		views = append(views, view)
	}
	return views
}

// heldInstances are the instances whose process is running and verified.
func (m *detachedSupervisorManager) heldInstances() []instanceView {
	var held []instanceView
	for _, v := range m.scanInstances() {
		if v.held && v.pid > 1 {
			held = append(held, v)
		}
	}
	return held
}

func (m *detachedSupervisorManager) track(path string) *instanceTrack {
	if m.instances == nil {
		m.instances = make(map[string]*instanceTrack)
	}
	t := m.instances[path]
	if t == nil {
		t = &instanceTrack{}
		m.instances[path] = t
	}
	return t
}

func (m *detachedSupervisorManager) startInstance(journal string, drain bool) error {
	m.lastSpawn = m.now()
	if m.spawn != nil {
		return m.spawn(journal, drain)
	}
	_, err := startSupervisorInstance(m.config, journal, drain)
	return err
}

// pass is one look at the machine's instances. It is the whole of the manager's policy:
//
//  1. an instance the server knows as retiring that holds its lock without a connection for retireDisconnectLimit, or that the server sees as
//     empty for retireEmptyLimit and that has not exited, gets the bounded handover (exit request, SIGTERM, SIGKILL of its tree);
//  2. a journal whose lock is free and that owes something is finished by a --drain process (a regular start when no instance is active, as
//     that is what takes the machine's new work); one that owes nothing is removed, except the legacy path;
//  3. when no instance is active and nothing is about to be, a new instance starts: on the legacy journal when it is free, on a fresh
//     journals/<name>.sqlite next to the others when it is held.
func (m *detachedSupervisorManager) pass(ctx context.Context) {
	m.instancesMu.Lock()
	defer m.instancesMu.Unlock()
	now := m.now()
	views := m.scanInstances()
	var conns map[string]bool
	if m.connections != nil {
		conns = m.connections()
	}
	activeUp := m.connected != nil && m.connected()
	states := m.instanceStates(ctx)
	unidentified := false
	blockers, unconnected, started := 0, 0, false
	legacyHeld := false
	var anyHeldPID int
	for _, v := range views {
		t := m.track(v.path)
		if v.journalID != "" {
			t.journalID = v.journalID
		}
		if !v.held {
			t.heldSince, t.unconnectedSince, t.emptySince = time.Time{}, time.Time{}, time.Time{}
			continue
		}
		if supervisor.IsLegacyJournal(v.path) {
			legacyHeld = true
		}
		if v.pid > 1 && anyHeldPID == 0 {
			anyHeldPID = v.pid
		}
		if v.journalID == "" {
			unidentified = true
		}
		if t.heldSince.IsZero() {
			t.heldSince = now
		}
		connected := v.journalID != "" && conns[v.journalID] || v.journalID == "" && len(conns) > 0
		if connected {
			t.unconnectedSince = time.Time{}
		} else {
			if t.unconnectedSince.IsZero() {
				t.unconnectedSince = now
			}
			unconnected++
			if now.Sub(t.unconnectedSince) < instanceStartWait {
				blockers++
			}
		}
		retiring := v.journalID != "" && states[v.journalID] == "retiring" || v.journalID == "" && activeUp
		if v.pid <= 1 || !retiring || now.Sub(t.handoverAt) < instanceHandoverAgain && !t.handoverAt.IsZero() {
			continue
		}
		switch {
		case !connected && now.Sub(t.unconnectedSince) >= retireDisconnectLimit:
			t.handoverAt = now
			log.Printf("supervisor swap: retiring instance pid %d holds its journal without a server connection for %s; handing over", v.pid, retireDisconnectLimit)
			if err := m.stopInstance(v, "liveness handover"); err != nil {
				fmt.Fprintln(m.stderr, "supervisor handover:", err)
			}
		case connected && v.journalID != "" && (m.busy != nil || m.store != nil && m.machineID != ""):
			active, err := m.instanceBusy(ctx, v.journalID)
			switch {
			case err != nil || active:
				t.emptySince = time.Time{}
			case t.emptySince.IsZero():
				t.emptySince = now
			case now.Sub(t.emptySince) >= retireEmptyLimit:
				t.handoverAt = now
				log.Printf("supervisor swap: retiring instance pid %d has nothing to run for %s and did not exit; handing over", v.pid, retireEmptyLimit)
				if err := m.stopInstance(v, "liveness handover"); err != nil {
					fmt.Fprintln(m.stderr, "supervisor handover:", err)
				}
			}
		}
	}
	for _, v := range views {
		if v.held {
			continue
		}
		t := m.track(v.path)
		if now.Sub(t.inspectedAt) < instanceInspectEvery || now.Sub(t.spawnedAt) < instanceSpawnGuard || now.Sub(m.lastSpawn) < instanceSpawnGuard {
			continue
		}
		t.inspectedAt = now
		state, err := supervisor.InspectJournal(ctx, v.path)
		if err != nil {
			continue // held by a process that started meanwhile
		}
		t.journalID = state.ID
		if !state.Owes {
			if !supervisor.IsLegacyJournal(v.path) {
				if removed, err := supervisor.RemoveJournal(ctx, v.path); err == nil && removed {
					log.Printf("supervisor swap: removed the empty journal %s of a supervisor instance that is gone", filepath.Base(v.path))
					delete(m.instances, v.path)
				}
			}
			continue
		}
		t.spawnedAt = now
		drain := activeUp || started
		if !drain {
			started = true
		}
		if drain {
			log.Printf("supervisor swap: journal %s still owes work and nobody holds it; starting a drain instance", filepath.Base(v.path))
		} else {
			log.Printf("supervisor swap: journal %s still owes work and no instance is active; starting a supervisor on it", filepath.Base(v.path))
		}
		if err := m.startInstance(v.path, drain); err != nil {
			fmt.Fprintln(m.stderr, "supervisor respawn:", err)
		}
	}
	if !activeUp && !started && blockers == 0 && unconnected < maxUnconnectedInstances && now.Sub(m.lastSpawn) >= instanceSpawnGuard {
		journal := ""
		if legacyHeld {
			path, err := supervisor.NewInstanceJournalPath(m.supervisorDir(), now)
			if err != nil {
				fmt.Fprintln(m.stderr, "supervisor respawn:", err)
				return
			}
			journal = path
			log.Printf("supervisor swap: no instance is active; starting a new one on %s next to pid %d", filepath.Base(path), anyHeldPID)
		}
		if err := m.startInstance(journal, false); err != nil {
			fmt.Fprintln(m.stderr, "supervisor respawn:", err)
		}
	}
	if !unidentified {
		m.writeOffVanished(ctx, now, views, conns)
	}
}

// instanceBusy says the server sees a turn or a running command owned by the instance.
func (m *detachedSupervisorManager) instanceBusy(ctx context.Context, journalID string) (bool, error) {
	if m.busy != nil {
		return m.busy(journalID)
	}
	return m.store.MachineActiveWorkForInstance(ctx, m.machineID, journalID)
}

// instanceStates is the server's state of each instance of the machine (active, retiring, gone), by journal ID.
func (m *detachedSupervisorManager) instanceStates(ctx context.Context) map[string]string {
	if m.states != nil {
		return m.states()
	}
	if m.store == nil || m.machineID == "" {
		return nil
	}
	list, err := m.store.SupervisorInstances(ctx, m.machineID)
	if err != nil {
		return nil
	}
	states := make(map[string]string, len(list))
	for _, item := range list {
		states[item.JournalID] = item.State
	}
	return states
}

// writeOffVanished tells the server an instance is gone when its journal no longer exists anywhere (deleted by hand or by a disk that was
// replaced) and it has not connected for a minute: what it still owned is lost, once, with the recovery the journal-change rule gives. An
// instance that retired cleanly removed its journal after the server had acknowledged everything, so it owns nothing and this only marks it.
func (m *detachedSupervisorManager) writeOffVanished(ctx context.Context, now time.Time, views []instanceView, conns map[string]bool) {
	if m.store == nil || m.machineID == "" {
		return
	}
	known := map[string]bool{}
	for _, v := range views {
		if v.journalID != "" {
			known[v.journalID] = true
		}
		if t := m.instances[v.path]; t != nil && t.journalID != "" {
			known[t.journalID] = true
		}
	}
	list, err := m.store.SupervisorInstances(ctx, m.machineID)
	if err != nil {
		return
	}
	for _, item := range list {
		if item.State == "gone" || known[item.JournalID] || conns[item.JournalID] {
			continue
		}
		seen, err := time.Parse(time.RFC3339Nano, item.LastSeenAt)
		if err != nil || now.Sub(seen) < instanceVanishedAfter {
			continue
		}
		turns, jobs, err := m.store.MarkSupervisorInstanceGone(ctx, m.machineID, item.JournalID)
		if err != nil {
			fmt.Fprintln(m.stderr, "supervisor instance write-off:", err)
			continue
		}
		if turns+jobs > 0 {
			log.Printf("supervisor swap: instance %s is gone with its journal; %d turn(s) and %d command(s) it owned were lost", item.JournalID, turns, jobs)
		}
	}
}
