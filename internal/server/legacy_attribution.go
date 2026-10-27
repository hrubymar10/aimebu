package server

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"time"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// legacyAttrTTL is how long what a transcript scan found for a v1 sender is kept; legacyAttrLimit is how many of the sender's latest message
// numbers are matched against it.
const (
	legacyAttrTTL   = 10 * time.Minute
	legacyAttrLimit = 2000
)

type legacyAttrEntry struct {
	at      time.Time
	session *protocol.SessionAttribution // nil: the scan ran and no session holds the sender's messages
}

// legacyAttrCall is a scan in flight: whoever asks for the same sender meanwhile waits for it instead of starting another.
type legacyAttrCall struct {
	done  chan struct{}
	entry legacyAttrEntry
	ran   bool
}

// resolveLegacyStart is Store.LegacyStart made as exact as the machine allows. v1 messages from a build that stamped them carry the session
// that sent them, and the store uses that. For the older ones this asks the host's supervisor which Claude Code session registered as the
// sender and sent its latest messages (ids from the transcripts' aimebu tool results; no text leaves the machine), and offers that session.
// What the scan cannot settle (an older supervisor, no host connection, no transcript holding the messages) leaves the store's answer, which
// is the session v1 kept under the name when the sender's messages do not contradict it.
func (s *Server) resolveLegacyStart(ctx context.Context, projectID, agentID string) (store.LegacyStart, error) {
	legacy, err := s.Store.LegacyStart(ctx, projectID, agentID)
	if err != nil || legacy.Exact || store.ResumableTool(legacy.Harness) == "" {
		return legacy, err
	}
	first, nos, err := s.Store.LegacySenderMessages(ctx, projectID, agentID, legacyAttrLimit)
	if err != nil || len(nos) == 0 {
		return legacy, err
	}
	entry, ran := s.legacySenderScan(ctx, projectID, agentID, legacy.CWD, first, nos)
	if !ran || entry.session == nil {
		return legacy, nil
	}
	found := entry.session
	legacy.SessionID, legacy.CWD, legacy.LastSeen = found.ID, found.CWD, found.ModifiedAt
	if found.Model != "" {
		legacy.Model = found.Model // the model of the session that did the work, not of the one v1 kept under the name
	}
	legacy.Tool, legacy.Mismatch, legacy.Exact = store.ResumableTool(legacy.Harness), "", true
	return legacy, nil
}

// legacySenderScan is what the transcripts say about a sender, from the cache while it is fresh. Only an answer from a scan that actually
// ran is kept: "there is no host connection yet", "the supervisor is old" or a scan that failed must not be remembered, or the next
// look (after the supervisor connects or is updated) would still see nothing. Asking for the same sender while a scan is in flight waits
// for that scan. ran is false when no scan answered.
func (s *Server) legacySenderScan(ctx context.Context, projectID, agentID, sessionCWD, first string, nos []int64) (legacyAttrEntry, bool) {
	key := projectID + "\x00" + agentID
	for {
		s.legacyAttrMu.Lock()
		if entry, ok := s.legacyAttr[key]; ok && time.Since(entry.at) <= legacyAttrTTL {
			s.legacyAttrMu.Unlock()
			return entry, true
		}
		if call, busy := s.legacyAttrCalls[key]; busy {
			s.legacyAttrMu.Unlock()
			select {
			case <-call.done:
				if call.ran {
					return call.entry, true
				}
				return legacyAttrEntry{}, false
			case <-ctx.Done():
				return legacyAttrEntry{}, false
			}
		}
		call := &legacyAttrCall{done: make(chan struct{})}
		if s.legacyAttrCalls == nil {
			s.legacyAttrCalls = make(map[string]*legacyAttrCall)
		}
		s.legacyAttrCalls[key] = call
		s.legacyAttrMu.Unlock()

		session, ran := s.scanLegacySender(ctx, projectID, agentID, sessionCWD, first, nos)
		call.entry, call.ran = legacyAttrEntry{at: time.Now(), session: session}, ran && ctx.Err() == nil
		s.legacyAttrMu.Lock()
		delete(s.legacyAttrCalls, key)
		if call.ran {
			if s.legacyAttr == nil {
				s.legacyAttr = make(map[string]legacyAttrEntry)
			}
			s.legacyAttr[key] = call.entry
		}
		s.legacyAttrMu.Unlock()
		close(call.done)
		return call.entry, call.ran
	}
}

// scanLegacySender asks the host's supervisor which session sent the sender's messages. ran is false when it could not ask (no host
// connection, a supervisor that predates attribution, no usable folder) or the scan did not answer; the session is nil when it answered and
// no session holds the sender's messages.
func (s *Server) scanLegacySender(ctx context.Context, projectID, agentID, sessionCWD, first string, nos []int64) (session *protocol.SessionAttribution, ran bool) {
	conn, machine, err := s.connectedMachine(ctx, "")
	if err != nil || !conn.supportsSessionAttribution {
		return nil, false
	}
	folder, err := s.Store.ProjectFolder(ctx, projectID)
	if err != nil || !filepath.IsAbs(folder) {
		return nil, false
	}
	folders := []string{folder}
	if real, err := filepath.EvalSymlinks(folder); err == nil && real != folder {
		folders = append(folders, real)
	}
	// The session may have worked below or beside the project's folder (a continued session keeps where it started): its recorded folder is
	// scanned too.
	if filepath.IsAbs(sessionCWD) && sessionCWD != folder {
		folders = append(folders, sessionCWD)
	}
	scan := protocol.Frame{ScanFolders: folders, ScanAttribution: true, ScanAgent: agentID}
	if at, err := time.Parse(time.RFC3339, first); err == nil {
		scan.ScanSince = at.Add(-time.Hour).UTC().Format(time.RFC3339)
	}
	reply, err := s.requestSessionScan(ctx, conn, scan, 30*time.Second)
	if err != nil {
		log.Printf("legacy start: the session scan on %s did not answer: %v", machine.Name, err)
		return nil, false
	}
	return bestAttribution(reply.Attributions, nos), true
}

// bestAttribution picks the session that sent the sender's latest message, or else the one that sent most of the numbers given (newest
// first); a session that sent none of them is not offered.
func bestAttribution(candidates []protocol.SessionAttribution, nos []int64) *protocol.SessionAttribution {
	if len(candidates) == 0 || len(nos) == 0 {
		return nil
	}
	mine := make(map[int64]bool, len(nos))
	for _, no := range nos {
		mine[no] = true
	}
	type scored struct {
		session         protocol.SessionAttribution
		holdsLatest     bool
		overlap         int
		newestMessageNo int64
	}
	var scores []scored
	for _, candidate := range candidates {
		item := scored{session: candidate}
		for _, no := range candidate.MessageNos {
			if mine[no] {
				item.overlap++
				item.newestMessageNo = max(item.newestMessageNo, no)
			}
			if no == nos[0] {
				item.holdsLatest = true
			}
		}
		if item.overlap > 0 {
			scores = append(scores, item)
		}
	}
	if len(scores) == 0 {
		return nil
	}
	sort.SliceStable(scores, func(i, j int) bool {
		a, b := scores[i], scores[j]
		// The session holding the latest message first; failing that, the one that sent the most of the sender's recent messages, then the
		// one with the newest of them, then the one written last.
		if a.holdsLatest != b.holdsLatest {
			return a.holdsLatest
		}
		if a.overlap != b.overlap {
			return a.overlap > b.overlap
		}
		if a.newestMessageNo != b.newestMessageNo {
			return a.newestMessageNo > b.newestMessageNo
		}
		return a.session.ModifiedAt > b.session.ModifiedAt
	})
	best := scores[0].session
	return &best
}
