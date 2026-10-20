package server

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/internal/store"
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
	return &Dispatcher{Server: server, lastRoom: make(map[string]string), inFlight: make(map[string]specialFlight), failures: make(map[string]dispatchFailure)}
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
	}
	d.Server.sendRoomControls(ctx, controls)
	d.scheduleCleanup(ctx)
	switchControls, err := d.Server.Store.AdvanceAgentSwitches(ctx)
	if err != nil {
		return 0, err
	}
	for _, card := range switchControls {
		if card.Desired == "not_accepting" {
			d.Server.sendRoomControls(ctx, []store.AgentCard{card})
			continue
		}
		if machine, err := d.Server.machineForID(card.MachineID); err == nil {
			if pending, ok, err := d.Server.Store.PendingSpawnByID(ctx, card.ID); err == nil && ok {
				if err := d.Server.sendPendingSpawn(ctx, machine, pending); err != nil {
					log.Printf("agent switch spawn %s: %v", card.ID, err)
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
		if _, err := d.Server.machineForID(agent.MachineID); err != nil {
			continue
		}
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
		}
	}()
}

func (d *Dispatcher) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	if err := d.Server.Store.SweepStagedAttachments(ctx); err != nil && onError != nil {
		onError(err)
	}
	for {
		if _, err := d.Server.Store.SweepUndispatchedNormalTurns(ctx); err == nil {
			break
		} else if onError != nil && ctx.Err() == nil {
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
	for {
		if !d.Server.maintenance.Load() && time.Since(lastAttachmentSweep) >= time.Hour {
			d.Server.maintenanceMu.RLock()
			if d.Server.maintenance.Load() {
				d.Server.maintenanceMu.RUnlock()
				continue
			}
			if err := d.Server.Store.SweepStagedAttachments(ctx); err != nil && ctx.Err() == nil && onError != nil {
				onError(err)
			}
			d.Server.maintenanceMu.RUnlock()
			lastAttachmentSweep = time.Now()
		}
		if _, err := d.Pass(ctx); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
