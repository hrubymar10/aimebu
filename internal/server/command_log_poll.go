package server

import (
	"crypto/sha256"
	"sync"
)

// commandLogPoller keeps only the last tail seen for a job in an agent's
// current turn. It is advice, not a rate limit; losing an entry is harmless.
type commandLogPoller struct {
	mu   sync.Mutex
	seen map[commandLogPollKey]commandLogPollState
}

type commandLogPollKey struct {
	agentID string
	jobID   string
}

type commandLogPollState struct {
	turnID  string
	digest  [32]byte
	repeats int
}

func (p *commandLogPoller) observe(agentID, turnID, jobID, state, log string) string {
	key := commandLogPollKey{agentID, jobID}
	p.mu.Lock()
	defer p.mu.Unlock()
	if state != "running" {
		delete(p.seen, key)
		return ""
	}
	if p.seen == nil || len(p.seen) >= 256 {
		p.seen = make(map[commandLogPollKey]commandLogPollState)
	}
	digest := sha256.Sum256([]byte(log))
	previous, found := p.seen[key]
	if !found || previous.turnID != turnID || previous.digest != digest {
		p.seen[key] = commandLogPollState{turnID: turnID, digest: digest}
		return ""
	}
	previous.repeats++
	p.seen[key] = previous
	if previous.repeats >= 3 {
		return "Stop polling this job and end your turn. The job is still running; you will be woken when it finishes."
	}
	return "The job is still running; end your turn, you will be woken when it finishes."
}
