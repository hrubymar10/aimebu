package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/store"
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
	// drainBackoff is how long the manager waits before it starts another drain for a debt a previous drain did not change: a drain that
	// leaves the journal exactly as it found it is not making progress, and an endless round of them spins at the drain's own pace.
	drainBackoff = time.Hour
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
	identityLogged                          bool   // the one log line about why this holder without an ID in its lock stays unidentified was written
	// What a started drain was left to finish, and when: a later pass that finds the journal free again and owing exactly that knows the
	// drain ended without progress, and backs off instead of starting another (the endless drain loop of A13-13).
	drainDebt      string
	drainStartedAt time.Time
	// The back-off that rule led to: until when no drain starts for the same debt, and whether this episode's one log line and notice went out.
	backoffUntil  time.Time
	backoffDebt   string
	backoffLogged bool
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
	if m.store != nil && m.machineID != "" {
		host, err := m.store.MachineByID(ctx, m.machineID)
		if err != nil {
			fmt.Fprintln(m.stderr, "supervisor scope:", err)
			return
		}
		if host.IsHost && host.Disabled {
			if m.disabledStopped == nil {
				m.disabledStopped = map[int]bool{}
			}
			for _, v := range m.heldInstances() {
				if !m.disabledStopped[v.pid] {
					if err := m.stopInstanceSignal(v, "host disabled", false); err != nil {
						fmt.Fprintln(m.stderr, "supervisor disable:", err)
					} else {
						m.disabledStopped[v.pid] = true
					}
				}
			}
			return
		}
		m.disabledStopped = nil
	}
	now := m.now()
	views := m.scanInstances()
	var conns map[string]bool
	if m.connections != nil {
		conns = m.connections()
	}
	activeUp := m.connected != nil && m.connected()
	states := m.instanceStates(ctx)
	identity := m.newHolderIdentity(views, conns)
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
			t.identityLogged = false
			continue
		}
		if supervisor.IsLegacyJournal(v.path) {
			legacyHeld = true
		}
		if v.pid > 1 && anyHeldPID == 0 {
			anyHeldPID = v.pid
		}
		// The journal ID the rules act on: the lock file's, or the one the holder without an ID in its lock was matched to. An unmatched
		// holder is left to its own devices: no rule of ours may cut it on a guess.
		id := v.journalID
		if id == "" {
			unidentified = true
			if v.pid > 1 {
				if matched, why, ok := m.identifyHolder(ctx, identity, conns); ok {
					id = matched
					t.identityLogged = false
				} else if !t.identityLogged {
					t.identityLogged = true
					log.Printf("supervisor swap: instance pid %d holds %s without a journal ID in its lock file; %s; leaving it alone", v.pid, filepath.Base(v.path), why)
				}
			}
		}
		if t.heldSince.IsZero() {
			t.heldSince = now
		}
		connected := id != "" && conns[id]
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
		retiring := id != "" && states[id] == "retiring"
		if v.pid <= 1 || !retiring || now.Sub(t.handoverAt) < instanceHandoverAgain && !t.handoverAt.IsZero() {
			continue
		}
		switch {
		case !connected && now.Sub(t.unconnectedSince) >= retireDisconnectLimit:
			t.handoverAt = now
			log.Printf("supervisor swap: retiring instance pid %d holds its journal without a server connection for %s; handing over", v.pid, retireDisconnectLimit)
			if err := m.stopInstanceSignal(v, "liveness handover", v.journalID != ""); err != nil {
				fmt.Fprintln(m.stderr, "supervisor handover:", err)
			}
		case connected && id != "" && (m.busy != nil || m.store != nil && m.machineID != ""):
			active, err := m.instanceBusy(ctx, id)
			switch {
			case err != nil || active:
				t.emptySince = time.Time{}
			case t.emptySince.IsZero():
				t.emptySince = now
			case now.Sub(t.emptySince) >= retireEmptyLimit:
				t.handoverAt = now
				log.Printf("supervisor swap: retiring instance pid %d has nothing to run for %s and did not exit; handing over", v.pid, retireEmptyLimit)
				if err := m.stopInstanceSignal(v, "liveness handover", v.journalID != ""); err != nil {
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
			// Whatever a drain left is gone: the next debt, if one comes, is a new episode.
			t.drainDebt, t.drainStartedAt = "", time.Time{}
			t.backoffUntil, t.backoffDebt, t.backoffLogged = time.Time{}, "", false
			continue
		}
		debt := state.Debt
		drain := activeUp || started
		if drain && !t.drainStartedAt.IsZero() && now.Sub(t.drainStartedAt) >= instanceSpawnGuard && t.drainDebt == debt.Signature() {
			// The drain this debt was left to has ended and the journal owes exactly what it did before: one more drain right away would
			// only loop. Back off, say once what is owed, and let a person see it.
			if t.backoffDebt != t.drainDebt || t.backoffUntil.IsZero() {
				t.backoffUntil, t.backoffDebt, t.backoffLogged = now.Add(drainBackoff), t.drainDebt, false
				log.Printf("supervisor swap: journal %s still owes the same work a drain instance was left (%s); backing off until %s before another",
					filepath.Base(v.path), debt, t.backoffUntil.Format(time.RFC3339))
				m.recordStuckJournal(ctx, v.path, debt.String())
			}
			if now.Before(t.backoffUntil) {
				continue
			}
			// The back-off is over: one more drain is tried for the same debt.
		}
		t.spawnedAt = now
		if !drain {
			started = true
		}
		if drain {
			t.drainDebt, t.drainStartedAt = debt.Signature(), now
			t.backoffUntil, t.backoffDebt, t.backoffLogged = time.Time{}, "", false
			log.Printf("supervisor swap: journal %s still owes work and nobody holds it (%s); starting a drain instance", filepath.Base(v.path), debt)
		} else {
			log.Printf("supervisor swap: journal %s still owes work and no instance is active (%s); starting a supervisor on it", filepath.Base(v.path), debt)
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

// recordStuckJournal records the one system event a person sees when a drain ended and the journal still owes the same work: the manager is
// backing off instead of starting another right away, and the debt says what a look at the journal has to settle. Where the manager has no
// server store (its tests), a hook stands in.
func (m *detachedSupervisorManager) recordStuckJournal(ctx context.Context, journalPath, debt string) {
	if m.stuckNotice != nil {
		m.stuckNotice(filepath.Base(journalPath), debt)
		return
	}
	if m.store == nil || m.machineID == "" {
		return
	}
	if err := m.store.RecordSupervisorJournalStuck(ctx, m.machineID, filepath.Base(journalPath), debt); err != nil {
		fmt.Fprintln(m.stderr, "supervisor journal notice:", err)
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
	records := m.instanceRecords(ctx)
	states := make(map[string]string, len(records))
	for _, item := range records {
		states[item.JournalID] = item.State
	}
	return states
}

// instanceRecords is the server's record of each instance of the machine, build included: what identifies a holder whose lock file has no ID.
func (m *detachedSupervisorManager) instanceRecords(ctx context.Context) []store.SupervisorInstance {
	if m.recordsOf != nil {
		return m.recordsOf()
	}
	if m.store == nil || m.machineID == "" {
		return nil
	}
	list, err := m.store.SupervisorInstances(ctx, m.machineID)
	if err != nil {
		return nil
	}
	return list
}

// holderIdentity works out which server instance a holder without an ID in its lock file (an older build on the legacy journal) is, without
// opening its journal. The holder's own connection carries the journal ID of its hello, so when it is connected, its is the one connection
// whose ID no lock file on the machine claims (the current build always writes its ID next to the pid). When it is not connected, the only
// sound left is the server's records: a retiring instance of an older build that no lock file claims and that is not connected either. Both
// rules name exactly one instance or they refuse: with anything ambiguous the holder is left alone, because cutting it on a guess could cut
// the wrong instance, which no rule here may do.
type holderIdentity struct {
	claimed  map[string]bool // the journal IDs this pass's lock files carry
	idless   int             // held holders without an ID in their lock file
	connsIDs []string        // the connections whose ID no lock file claims, sorted
	records  []store.SupervisorInstance
	connsUse bool
	recsUse  bool
}

func (m *detachedSupervisorManager) newHolderIdentity(views []instanceView, conns map[string]bool) *holderIdentity {
	h := &holderIdentity{claimed: map[string]bool{}}
	for _, v := range views {
		if v.held && v.journalID != "" {
			h.claimed[v.journalID] = true
		}
		if v.held && v.journalID == "" {
			h.idless++
		}
	}
	return h
}

// identify names the server instance of one ID-less holder, or says why it cannot. It is only asked when exactly one holder on the machine
// lacks an ID: with more, no connection or record can be attributed to one of them.
func (m *detachedSupervisorManager) identifyHolder(ctx context.Context, h *holderIdentity, conns map[string]bool) (id, why string, ok bool) {
	if h.idless != 1 {
		return "", fmt.Sprintf("%d held journals have no ID in their lock file, so none can be told from the others", h.idless), false
	}
	if !h.connsUse {
		h.connsUse = true
		for id := range conns {
			if !h.claimed[id] {
				h.connsIDs = append(h.connsIDs, id)
			}
		}
		sort.Strings(h.connsIDs)
	}
	switch len(h.connsIDs) {
	case 1:
		return h.connsIDs[0], "", true
	case 0:
		// The holder is not connected (or its connection is one whose ID a lock claims, which cannot be). The server's records are left:
		// an instance that is retiring, of a build that is not this manager's (an older build is the only kind that leaves no ID in its
		// lock), that no lock file claims and that is not connected either.
		if !h.recsUse {
			h.recsUse = true
			h.records = m.instanceRecords(ctx)
		}
		var candidates []string
		for _, item := range h.records {
			if item.State != "retiring" || item.Build == version || h.claimed[item.JournalID] || conns[item.JournalID] {
				continue
			}
			candidates = append(candidates, item.JournalID)
		}
		switch len(candidates) {
		case 1:
			return candidates[0], "", true
		case 0:
			return "", "no connection whose journal no lock file names and no retiring record of an older build is left once every lock file's ID is accounted for", false
		default:
			return "", fmt.Sprintf("%d retiring records of older builds are left once every lock file's ID is accounted for", len(candidates)), false
		}
	default:
		return "", fmt.Sprintf("%d connected instances have a journal no lock file names, so the holder without an ID cannot be told from them", len(h.connsIDs)), false
	}
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
