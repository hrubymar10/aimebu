package supervisor

import (
	"errors"
	"time"

	"github.com/hrubymar10/aimebu/internal/ccsessions"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

// sessionScanResult answers a session_scan frame from the Claude state of the launcher that would continue the session (its own
// CLAUDE_CONFIG_DIR / HOME references apply): the sessions that worked in a folder, or one session by id. It is the configuration folder
// the Claude adapter runs in, so what it lists is what `claude --resume` can continue.
func (c *Client) sessionScanResult(scan protocol.Frame) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.SessionScanResult, ReqID: scan.ReqID}
	var refs map[string]string
	if scan.LauncherID != "" {
		row := c.launcherRow(scan.LauncherID)
		if row.ID == "" {
			result.Code, result.Message = "launcher_unavailable", "launcher is not configured on this machine"
			return result
		}
		refs = row.EnvRefs
	}
	dir := c.ClaudeConfigDir
	if dir == "" {
		dir = ccsessions.ConfigDirWith(refs)
	}
	var found []ccsessions.Session
	switch {
	case scan.ScanSessionID != "" && len(scan.ScanFolders) == 0:
		session, err := ccsessions.Find(dir, scan.ScanSessionID)
		if errors.Is(err, ccsessions.ErrNotFound) {
			result.Code, result.Message = "session_not_found", "this machine has no Claude Code session with that id"
			return result
		}
		if errors.Is(err, ccsessions.ErrUnreadable) {
			result.Code, result.Message = "session_unreadable", "that session's transcript has no readable working folder"
			return result
		}
		if err != nil {
			result.Code, result.Message = "scan_failed", err.Error()
			return result
		}
		found = []ccsessions.Session{session}
	case len(scan.ScanFolders) > 0 && len(scan.ScanFolders) <= 4 && scan.ScanSessionID == "":
		scanned, err := ccsessions.Scan(dir, scan.ScanFolders...)
		if err != nil {
			result.Code, result.Message = "scan_failed", err.Error()
			return result
		}
		found = scanned.Sessions
		result.Unreadable, result.ScanTruncated = scanned.Unreadable, scanned.Truncated
	default:
		result.Code, result.Message = "invalid_request", "name one to four spellings of a folder, or a session id, not both"
		return result
	}
	for _, session := range found {
		result.Sessions = append(result.Sessions, protocol.SessionCandidate{ID: session.ID, CWD: session.CWD, Dir: session.Dir, Title: session.Title,
			GitBranch: session.GitBranch, Version: session.Version, Entrypoint: session.Entrypoint,
			ModifiedAt: session.ModifiedAt.UTC().Format(time.RFC3339), Size: session.Size, Active: session.Active})
	}
	return result
}
