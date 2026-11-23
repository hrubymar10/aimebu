package credtarget

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/goccy/go-json"
)

// claudeMCPSection is the object of .credentials.json that holds the OAuth logins of
// the user's MCP servers, keyed by server. It is not part of the Claude account login
// and not owned by a profile: a login made to an MCP server must survive a profile
// switch, so the switcher carries it from the live file to the profile it activates.
const claudeMCPSection = "mcpOAuth"

// MCPMerge is the outcome of MergeClaudeMCPOAuth.
type MCPMerge struct {
	// Merged is the incoming document with the live MCP logins merged in; identical to
	// the incoming bytes when there was nothing to carry.
	Merged []byte
	// Conflicts are the server keys present on both sides with different content. The
	// live side won for each. Keys only, never values.
	Conflicts []string
	// Replaced reports that the incoming document's own mcpOAuth section was unusable
	// (not an object) and was replaced by the live one.
	Replaced bool
}

// LosesIncoming reports whether the merge overwrote content the incoming document had,
// which is when the caller must keep a copy of the incoming bytes first.
func (m MCPMerge) LosesIncoming() bool { return len(m.Conflicts) > 0 || m.Replaced }

// MergeClaudeMCPOAuth carries the MCP OAuth logins of live (the active, running
// credential file) into incoming (the full credential of the profile being activated):
// the union of both maps, and where the same server key holds different content the
// live entry wins (it is the session the user was just using; freshness is deliberately
// not inferred from expiry). Every other member of incoming, the account login above
// all, is kept byte for byte, so no refresh token of the live account reaches the
// incoming profile.
//
// A live document without a usable mcpOAuth section has nothing to carry: incoming is
// returned unchanged and no error is raised. An unreadable live mcpOAuth section is an
// error, which the caller reports and then switches without carrying anything.
func MergeClaudeMCPOAuth(incoming, live []byte) (MCPMerge, error) {
	liveEntries, err := claudeMCPEntries(live)
	if err != nil {
		return MCPMerge{}, fmt.Errorf("live credentials: %w", err)
	}
	if liveEntries == nil || len(liveEntries.members) == 0 {
		return MCPMerge{Merged: incoming}, nil
	}
	doc, err := parseObject(incoming)
	if err != nil {
		return MCPMerge{}, fmt.Errorf("incoming credentials: %w", err)
	}
	var result MCPMerge
	var section *object
	if raw, ok := doc.value(claudeMCPSection); ok && !isJSONNull(raw) {
		if section, err = parseObject(raw); err != nil {
			section = nil
			result.Replaced = true
		}
	}
	if section == nil {
		doc.set(claudeMCPSection, liveEntries.bytes())
		result.Merged = doc.bytes()
		return result, nil
	}
	for _, m := range liveEntries.members {
		have, ok := section.value(m.name)
		switch {
		case !ok:
			section.set(m.name, m.value)
		case !sameJSON(have, m.value):
			section.set(m.name, m.value)
			result.Conflicts = append(result.Conflicts, m.name)
		}
	}
	sort.Strings(result.Conflicts)
	doc.set(claudeMCPSection, section.bytes())
	result.Merged = doc.bytes()
	return result, nil
}

// ClaudeMCPOnly reduces a Claude credential document to its MCP OAuth logins: the
// account login and every other member are dropped. ok is false when the document has
// no usable mcpOAuth entries (absent, null, empty, unreadable), in which case there is
// nothing to keep and the caller removes the file as it always did.
//
// It is what a live file becomes when the active profile switches to an empty one: the
// outgoing account's tokens must go, the user's MCP logins must not.
func ClaudeMCPOnly(data []byte) (only []byte, ok bool) {
	entries, err := claudeMCPEntries(data)
	if err != nil || entries == nil || len(entries.members) == 0 {
		return nil, false
	}
	return []byte(`{"` + claudeMCPSection + `":` + string(entries.bytes()) + "}\n"), true
}

// claudeMCPEntries returns the parsed mcpOAuth object of a credential document, nil
// when the document has none (missing member, null or empty file). A document or
// section that is present but not an object is an error.
func claudeMCPEntries(data []byte) (*object, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	doc, err := parseObject(data)
	if err != nil {
		return nil, err
	}
	raw, ok := doc.value(claudeMCPSection)
	if !ok || isJSONNull(raw) {
		return nil, nil
	}
	entries, err := parseObject(raw)
	if err != nil {
		return nil, errors.New("mcpOAuth is not a JSON object")
	}
	return entries, nil
}

func isJSONNull(raw []byte) bool { return string(bytes.TrimSpace(raw)) == "null" }

// sameJSON reports whether two JSON values are the same apart from insignificant whitespace.
func sameJSON(a, b []byte) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// LockStorage takes Claude's `<dir>/.storage-write.lock` for a caller that has to hold it
// across several steps (the switcher reading and replacing the live file). dir must exist.
// release lets go of the lock and reports whether its heartbeat failed while it was held,
// in which case the exclusion was not guaranteed for the whole time.
func (w *Writer) LockStorage(dir string) (release func() (lost bool), err error) {
	lock, err := w.lockStorage(dir)
	if err != nil {
		return nil, err
	}
	return lock.release, nil
}

// LockRefresh takes Claude's `<dir>/.oauth_refresh.lock`, the lock a Claude process holds while it refreshes its OAuth token, the same way LockStorage
// takes the storage-write lock. The switcher holds it across the file swap so a refresh in flight finishes before the swap and a new one starts after
// it: Claude re-reads the file before every refresh, so it then refreshes the incoming account's token, never the outgoing one's. When both are
// taken, this one comes first (Claude's own order). dir must exist.
func (w *Writer) LockRefresh(dir string) (release func() (lost bool), err error) {
	lock, err := w.lockDir(dir, OAuthRefreshLockPath)
	if err != nil {
		return nil, err
	}
	return lock.release, nil
}

// ClearTarget removes the account login from t. A Claude target keeps its MCP OAuth
// logins (ClaudeMCPOnly) and is removed outright only when it has none; every other
// target is removed. A target that is already gone is cleared. It never destroys an
// unimported login: a refresh token on disk returns ErrRefreshTokenPresent and changes
// nothing. The Claude check and rewrite run under the storage-write lock, so an MCP
// login the CLI writes meanwhile is not lost.
func (w *Writer) ClearTarget(t Target) (err error) {
	if t.Tool != ToolClaude && t.Tool != ToolCodex {
		return fmt.Errorf("credtarget: unknown tool %q", t.Tool)
	}
	if t.Tool == ToolClaude {
		dir := filepath.Dir(t.Path)
		if _, statErr := os.Stat(dir); statErr == nil {
			lock, lockErr := w.lockStorage(dir)
			if lockErr != nil {
				return lockErr
			}
			defer func() {
				if lost := lock.release(); lost && err == nil {
					err = ErrLockLost
				}
			}()
		}
	}
	existing, readErr := os.ReadFile(t.Path)
	switch {
	case errors.Is(readErr, fs.ErrNotExist):
		return nil
	case readErr != nil:
		return fmt.Errorf("%s: cannot check for an unimported login: %w", t.ID, readErr)
	case refreshTokenOnDisk(t.Tool, existing):
		return fmt.Errorf("%w: %s", ErrRefreshTokenPresent, t.Path)
	}
	if t.Tool == ToolClaude {
		if only, ok := ClaudeMCPOnly(existing); ok {
			if bytes.Equal(only, existing) {
				return nil
			}
			info, _ := os.Stat(t.Path)
			return writeAtomic(t.Path, only, info)
		}
	}
	if rmErr := os.Remove(t.Path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return rmErr
	}
	return nil
}
