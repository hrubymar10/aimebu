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
	key := projectID + "\x00" + agentID
	s.legacyAttrMu.Lock()
	entry, cached := s.legacyAttr[key]
	s.legacyAttrMu.Unlock()
	if !cached || time.Since(entry.at) > legacyAttrTTL {
		entry = legacyAttrEntry{at: time.Now(), session: s.scanLegacySender(ctx, projectID, agentID, legacy.CWD, first, nos)}
		if entry.session != nil || ctx.Err() == nil {
			s.legacyAttrMu.Lock()
			if s.legacyAttr == nil {
				s.legacyAttr = make(map[string]legacyAttrEntry)
			}
			s.legacyAttr[key] = entry
			s.legacyAttrMu.Unlock()
		}
	}
	if entry.session == nil {
		return legacy, nil
	}
	found := entry.session
	legacy.SessionID, legacy.CWD, legacy.LastSeen = found.ID, found.CWD, found.ModifiedAt
	legacy.Tool, legacy.Mismatch, legacy.Exact = store.ResumableTool(legacy.Harness), "", true
	return legacy, nil
}

// scanLegacySender asks the host's supervisor which session sent the sender's messages. nil when it cannot say.
func (s *Server) scanLegacySender(ctx context.Context, projectID, agentID, sessionCWD, first string, nos []int64) *protocol.SessionAttribution {
	conn, machine, err := s.connectedMachine(ctx, "")
	if err != nil || !conn.supportsSessionAttribution {
		return nil
	}
	folder, err := s.Store.ProjectFolder(ctx, projectID)
	if err != nil || !filepath.IsAbs(folder) {
		return nil
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
		return nil
	}
	return bestAttribution(reply.Attributions, nos)
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
		if a.holdsLatest != b.holdsLatest {
			return a.holdsLatest
		}
		if a.newestMessageNo != b.newestMessageNo {
			return a.newestMessageNo > b.newestMessageNo
		}
		if a.overlap != b.overlap {
			return a.overlap > b.overlap
		}
		return a.session.ModifiedAt > b.session.ModifiedAt
	})
	best := scores[0].session
	return &best
}
