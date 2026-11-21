package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// Dispatcher makes one serial pass. Machine delivery can fail after durable
// preparation; in that case the never-sent turn is removed and its notes stay
// queued. Once queued on the socket, journal reconciliation owns uncertainty.
type Dispatcher struct {
	Server                 *Server
	mu                     sync.Mutex
	lastRoom               map[string]string
	inFlight               map[string]specialFlight
	failures               map[string]dispatchFailure
	dockerHeld             map[string]bool         // agents held by Only run agents in docker (a native launcher), said once in their rooms
	dockerUnchecked        map[string]bool         // agents held because their launcher could not be read, said once (apart from dockerHeld, so the native-launcher note is never swallowed)
	forkHeld               map[string]*machineConn // agents whose copy-of-a-session turn waits for a supervisor that can fork, and the connection told so
	lastProviderLimitSweep time.Time
	cleanupMu              sync.Mutex
	cleanupRunning         bool
	lastCleanup            time.Time
}

type dispatchFailure struct {
	count   int
	retryAt time.Time
}
type specialFlight struct {
	connection *machineConn
	sentAt     time.Time
}

func dispatchBackoff(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	if failures > 8 {
		failures = 8
	}
	delay := 200 * time.Millisecond * time.Duration(1<<(failures-1))
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func NewDispatcher(server *Server) *Dispatcher {
	return &Dispatcher{Server: server, lastRoom: make(map[string]string), inFlight: make(map[string]specialFlight), failures: make(map[string]dispatchFailure), forkHeld: make(map[string]*machineConn), dockerHeld: make(map[string]bool), dockerUnchecked: make(map[string]bool)}
}

func (d *Dispatcher) Pass(ctx context.Context) (int, error) {
	if d.Server.maintenance.Load() {
		return 0, nil
	}
	d.Server.maintenanceMu.RLock()
	defer d.Server.maintenanceMu.RUnlock()
	if d.Server.maintenance.Load() {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.lastProviderLimitSweep) >= time.Minute {
		if err := d.Server.Store.AdvanceProviderLimits(ctx); err != nil {
			return 0, err
		}
		d.lastProviderLimitSweep = time.Now()
	}
	if err := d.Server.Store.AdvanceUsageDays(ctx); err != nil {
		return 0, err
	}
	if _, err := d.Server.Store.AdvanceSchedules(ctx, time.Now()); err != nil {
		return 0, err
	}
	controls, failures, err := d.Server.Store.AdvanceRoomClosures(ctx, 30*time.Second)
	if err != nil {
		return 0, err
	}
	for _, failure := range failures {
		log.Printf("room close %s: %v", failure.RoomID, failure.Err)
		telemetry.ReportError(ctx, "dispatcher", fmt.Errorf("room close: %w", failure.Err))
	}
	d.Server.sendRoomControls(ctx, controls)
	d.scheduleCleanup(ctx)
	migrationControls, err := d.Server.Store.AdvanceTeamFlowMigrations(ctx)
	if err != nil {
		return 0, err
	}
	for _, card := range migrationControls {
		if card.Desired == "stopped" || card.Desired == "not_accepting" {
			d.Server.sendRoomControls(ctx, []store.AgentCard{card})
			continue
		}
		if machine, err := d.Server.machineForDispatch(card.MachineID); err == nil {
			if pending, ok, err := d.Server.Store.PendingSpawnByID(ctx, card.ID); err == nil && ok {
				if err := d.Server.sendPendingSpawn(ctx, machine, pending); err != nil {
					log.Printf("team flow migration spawn %s: %v", card.ID, err)
					telemetry.ReportError(ctx, "dispatcher", fmt.Errorf("team flow migration spawn: %w", err))
				}
			}
		}
	}
	switchControls, err := d.Server.Store.AdvanceAgentSwitches(ctx)
	if err != nil {
		return 0, err
	}
	for _, card := range switchControls {
		if card.Desired == "not_accepting" {
			d.Server.sendRoomControls(ctx, []store.AgentCard{card})
			continue
		}
		if machine, err := d.Server.machineForDispatch(card.MachineID); err == nil {
			if pending, ok, err := d.Server.Store.PendingSpawnByID(ctx, card.ID); err == nil && ok {
				if err := d.Server.sendPendingSpawn(ctx, machine, pending); err != nil {
					log.Printf("agent switch spawn %s: %v", card.ID, err)
					telemetry.ReportError(ctx, "dispatcher", fmt.Errorf("agent switch spawn: %w", err))
				}
			}
		}
	}
	clearControls, err := d.Server.Store.AdvanceAgentContextClears(ctx)
	if err != nil {
		return 0, err
	}
	for _, card := range clearControls {
		if machine, err := d.Server.machineForDispatch(card.MachineID); err == nil {
			if pending, ok, err := d.Server.Store.PendingSpawnByID(ctx, card.ID); err == nil && ok {
				if err := d.Server.sendPendingSpawn(ctx, machine, pending); err != nil {
					log.Printf("context clear spawn %s: %v", card.ID, err)
				}
			}
		}
	}
	if _, err := d.Server.Store.ExpireBlockedSpecialTurns(ctx); err != nil {
		return 0, err
	}
	for turnID, flight := range d.inFlight {
		if time.Since(flight.sentAt) > 5*time.Second {
			delete(d.inFlight, turnID)
			continue
		}
		state, err := d.Server.Store.DeliveryState(ctx, turnID)
		if err != nil || state.Exec != "pending" {
			delete(d.inFlight, turnID)
			continue
		}
		agentID, _, err := d.Server.Store.TurnBinding(ctx, turnID)
		if err != nil {
			delete(d.inFlight, turnID)
			continue
		}
		agent, err := d.Server.Store.AgentRuntimeByID(ctx, agentID)
		if err != nil {
			delete(d.inFlight, turnID)
			continue
		}
		current, err := d.Server.machineForID(agent.MachineID)
		if err != nil || current != flight.connection {
			delete(d.inFlight, turnID)
		}
	}
	ids, err := d.Server.Store.QueuedAgentIDs(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		if failure := d.failures[id]; time.Now().Before(failure.retryAt) {
			continue
		}
		agent, err := d.Server.Store.AgentRuntimeByID(ctx, id)
		if err != nil {
			return count, err
		}
		machine, err := d.Server.machineForDispatch(agent.MachineID)
		if err != nil {
			continue
		}
		// A turn that forks a Claude Code session waits for a supervisor that can: asked before the turn is prepared, so the wait does not
		// churn turns, and said once in the agent's room.
		// The same wait for a turn that must start Claude in the spelling its session was saved under.
		// And for a turn on a custom OpenCode provider, which an older supervisor would start without the endpoint.
		if !machine.supportsSessionFork || !machine.supportsLaunchPath || !machine.supportsProviders || !machine.supportsLegacySessionCopy {
			// A failure here is this agent's, not the pass's: it is logged, the agent waits (fails closed, nothing is sent), and the
			// other agents go on.
			why := ""
			if !machine.supportsSessionFork {
				pending, err := d.Server.Store.AgentForkPending(ctx, id)
				if err != nil {
					log.Printf("dispatch: is %s a copy of a session still unnamed: %v", id, err)
					telemetry.ReportWarning(ctx, "dispatcher", fmt.Errorf("dispatch hold check: is an agent a copy of a session still unnamed: %w", err))
					continue
				}
				if pending {
					why = store.HeldForFork
				}
			}
			if why == "" && !machine.supportsLaunchPath {
				pending, err := d.Server.Store.AgentLaunchPathPending(ctx, id)
				if err != nil {
					log.Printf("dispatch: does %s need a launch spelling: %v", id, err)
					telemetry.ReportWarning(ctx, "dispatcher", fmt.Errorf("dispatch hold check: does an agent need a launch spelling: %w", err))
					continue
				}
				if pending {
					why = store.HeldForLaunchPath
				}
			}
			if why == "" && !machine.supportsProviders {
				pending, err := d.Server.Store.AgentProviderPending(ctx, id)
				if err != nil {
					log.Printf("dispatch: does %s run on a custom provider: %v", id, err)
					telemetry.ReportWarning(ctx, "dispatcher", fmt.Errorf("dispatch hold check: does an agent run on a custom provider: %w", err))
					continue
				}
				if pending {
					why = store.HeldForProviders
				}
			}
			if why == "" && !machine.supportsLegacySessionCopy {
				pending, err := d.Server.Store.AgentLegacyCopyPending(ctx, id)
				if err != nil {
					log.Printf("dispatch: does %s need an imported session copy: %v", id, err)
					telemetry.ReportWarning(ctx, "dispatcher", fmt.Errorf("dispatch hold check: does an agent need an imported session copy: %w", err))
					continue
				}
				if pending {
					why = store.HeldForLegacyCopy
				}
			}
			if why != "" {
				if d.forkHeld[id] != machine {
					if err := d.Server.Store.NoteSessionHeld(ctx, id, why); err != nil {
						log.Printf("dispatch: saying why %s is held: %v", id, err)
						telemetry.ReportWarning(ctx, "dispatcher", fmt.Errorf("dispatch: saying why an agent is held: %w", err))
						continue
					}
					d.forkHeld[id] = machine
				}
				continue
			}
		}
		delete(d.forkHeld, id)
		// Only run agents in docker: an agent on a native launcher is held before a turn is made for it, so its queued messages wait without a turn row being
		// created and cancelled on every pass (Dispatch refuses it too, as a backstop). Said once in its rooms.
		refused, err := d.Server.refusedAgent(ctx, agent.MachineID, agent.Tool, agent.Variant, agent.LauncherID)
		if err != nil {
			// Held, not sent: with the setting on, an agent whose launcher cannot be read may be a native one. Said once in its rooms, logged on every pass.
			log.Printf("dispatch: is %s on a native launcher: %v", id, err)
			telemetry.ReportWarning(ctx, "dispatcher", fmt.Errorf("dispatch: is an agent on a native launcher: %w", err))
			if on, onErr := d.Server.dockerOnly(ctx); onErr == nil && on {
				if !d.dockerUnchecked[id] {
					if noteErr := d.Server.Store.NoteDockerOnlyUnchecked(ctx, id); noteErr != nil {
						log.Printf("dispatch: saying why %s is held: %v", id, noteErr)
					} else {
						d.dockerUnchecked[id] = true
					}
				}
				continue
			}
			continue
		}
		if refused {
			if !d.dockerHeld[id] {
				if err := d.Server.Store.NoteDockerOnlyHeld(ctx, id); err != nil {
					log.Printf("dispatch: saying why %s is held: %v", id, err)
					continue
				}
				d.dockerHeld[id] = true
			}
			continue
		}
		delete(d.dockerHeld, id)
		delete(d.dockerUnchecked, id)
		delivery, ready, err := d.Server.Store.PrepareDispatch(ctx, id, d.lastRoom[id])
		if err != nil {
			return count, err
		}
		if !ready {
			continue
		}
		if _, ok := d.inFlight[delivery.TurnID]; ok {
			continue
		}
		if err := d.Server.Dispatch(ctx, delivery); err != nil {
			admitted, checkErr := d.Server.Store.TurnDispatchRequested(ctx, delivery.TurnID)
			if checkErr != nil {
				return count, errors.Join(err, checkErr)
			}
			if admitted {
				// Enqueue may fail after durable admission. Journal/hello owns
				// that uncertainty; sending the same turn again can rerun it.
				delete(d.failures, id)
				continue
			}
			failure := d.failures[id]
			failure.count++
			failure.retryAt = time.Now().Add(dispatchBackoff(failure.count))
			d.failures[id] = failure
			// Said once per run of failures: the agent is retried with a backoff and nothing else shows why its messages wait
			// (a stale generation, for one, leaves both the server's and the supervisor's logs empty).
			if failure.count == 1 {
				log.Printf("dispatch to %s failed, retrying with a backoff: %v", id, err)
			}
			if delivery.Mode == "normal" && delivery.ParentTurnID == "" {
				if cancelErr := d.Server.Store.CancelPreparedDispatch(ctx, delivery.TurnID); cancelErr != nil {
					return count, errors.Join(err, cancelErr)
				}
			} else if delivery.Mode != "normal" && failure.count >= 3 {
				if abandonErr := d.Server.Store.AbandonSpecialTurn(ctx, delivery.TurnID, "dispatch_failed"); abandonErr != nil {
					return count, errors.Join(err, abandonErr)
				}
				delete(d.failures, id)
			}
			continue
		}
		delete(d.failures, id)
		if delivery.Mode != "normal" {
			connection, _ := d.Server.machineForID(agent.MachineID)
			d.inFlight[delivery.TurnID] = specialFlight{connection: connection, sentAt: time.Now()}
		}
		if len(delivery.Notes) > 0 {
			d.lastRoom[id] = delivery.Notes[0].RoomID
		}
		count++
	}
	return count, nil
}

// scheduleCleanup runs the daily cleanup when it is due. It never removes worktrees: that is a manual action in 2.0.
func (d *Dispatcher) scheduleCleanup(ctx context.Context) {
	d.cleanupMu.Lock()
	if d.cleanupRunning || time.Since(d.lastCleanup) < 30*time.Second {
		d.cleanupMu.Unlock()
		return
	}
	d.cleanupRunning = true
	d.lastCleanup = time.Now()
	d.cleanupMu.Unlock()
	go func() {
		defer telemetry.Recover(ctx)
		defer func() {
			d.cleanupMu.Lock()
			d.cleanupRunning = false
			d.cleanupMu.Unlock()
		}()
		if d.Server.maintenance.Load() {
			return
		}
		d.Server.maintenanceMu.RLock()
		defer d.Server.maintenanceMu.RUnlock()
		if d.Server.maintenance.Load() {
			return
		}
		scheduledCtx, cancelScheduled := context.WithTimeout(ctx, 15*time.Minute)
		defer cancelScheduled()
		if err := d.Server.scheduledCleanup(scheduledCtx); err != nil {
			log.Printf("scheduled cleanup: %v", err)
			telemetry.ReportError(ctx, "dispatcher", fmt.Errorf("scheduled cleanup: %w", err))
		}
	}()
}

func (d *Dispatcher) Run(ctx context.Context, interval time.Duration, caller func(error)) {
	// Every failure of a pass is said to the caller (a line on stderr) and to Sentry, once per cause: the pass runs every 100ms, so a failing one repeats at once.
	onError := func(err error) {
		telemetry.ReportError(ctx, "dispatcher", err)
		if caller != nil {
			caller(err)
		}
	}
	if err := d.Server.Store.SweepStagedAttachments(ctx); err != nil {
		onError(err)
	}
	for {
		if _, err := d.Server.Store.SweepUndispatchedNormalTurns(ctx); err == nil {
			break
		} else if ctx.Err() == nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastAttachmentSweep := time.Now()
	lastRecoverySweep := time.Time{}
	for {
		if !d.Server.maintenance.Load() && time.Since(lastRecoverySweep) >= 10*time.Second {
			d.Server.maintenanceMu.RLock()
			if !d.Server.maintenance.Load() {
				if err := d.Server.Store.SweepWorkerRecoveries(ctx); err != nil && ctx.Err() == nil {
					onError(err)
				}
				if err := d.Server.Store.SettleOrphanedInbox(ctx); err != nil && ctx.Err() == nil {
					onError(err)
				}
				if err := d.Server.sweepUnconfirmedCommands(ctx); err != nil && ctx.Err() == nil {
					onError(err)
				}
			}
			d.Server.maintenanceMu.RUnlock()
			lastRecoverySweep = time.Now()
		}
		if !d.Server.maintenance.Load() && time.Since(lastAttachmentSweep) >= time.Hour {
			d.Server.maintenanceMu.RLock()
			if d.Server.maintenance.Load() {
				d.Server.maintenanceMu.RUnlock()
				continue
			}
			if err := d.Server.Store.SweepStagedAttachments(ctx); err != nil && ctx.Err() == nil {
				onError(err)
			}
			d.Server.maintenanceMu.RUnlock()
			lastAttachmentSweep = time.Now()
		}
		if _, err := d.Pass(ctx); err != nil && ctx.Err() == nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
