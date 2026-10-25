// Package ccsessions finds the Claude Code sessions a machine already has, so that one of them can be continued as an aimebu agent. It reads
// only names and a bounded head and tail of each transcript, never a whole one, and it never writes. Claude keeps a session as a JSONL
// transcript under <config dir>/projects/<working folder, every non-alphanumeric turned into "-">/<session id>.jsonl, and registers each
// running interactive process in <config dir>/sessions/<pid>.json.
package ccsessions

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/goccy/go-json"
)

const (
	// MaxHead and MaxTail bound what is read of one transcript: the start (where the working folder, branch and first prompt are) and the
	// end (where Claude appends the title it made and the last prompt).
	MaxHead = 64 << 10
	MaxTail = 64 << 10
	// MaxHeadRetry is how far a head is read once more when its first window held no working folder (a first line longer than the window).
	MaxHeadRetry = 1 << 20
	// MaxInspect bounds how many transcripts one scan opens, newest first, and MaxResults how many sessions it returns. The bound on
	// transcripts counts what was opened, not what matched, so busy sibling folders cannot push a project's sessions out of the list.
	MaxInspect = 5000
	MaxResults = 200
	// MaxProbes bounds how many directories with a long name (one Claude may have shortened) are opened to learn their working folder.
	MaxProbes = 200
	// longDirName is the length from which a directory name that does not start like the folder's is looked into: Claude shortens a long
	// path and adds a hash, so the name then no longer says where it came from.
	longDirName = 100
	// TitleRunes is the longest title a session is shown with.
	TitleRunes = 120
)

// ErrNotFound is a session id no transcript of this machine carries.
var ErrNotFound = errors.New("claude session not found")

// Session is what is known of one transcript without reading it all.
type Session struct {
	ID  string `json:"id"`
	CWD string `json:"cwd"`
	// Dir is the name of the project directory the transcript sits in. Claude derives it from the working folder it was started in, and
	// looks a session up there, so it tells whether a session can be continued from some other spelling of that folder.
	Dir        string    `json:"dir"`
	Title      string    `json:"title,omitempty"`
	GitBranch  string    `json:"git_branch,omitempty"`
	Version    string    `json:"version,omitempty"`
	Entrypoint string    `json:"entrypoint,omitempty"`
	ModifiedAt time.Time `json:"modified_at"`
	Size       int64     `json:"size"`
	// Active says a live Claude process registered this session (it is open in a terminal, or an agent is running it). Advisory: the
	// registry is the only evidence there is.
	Active bool `json:"active,omitempty"`
}

// ConfigDir is where this process's Claude keeps its state: CLAUDE_CONFIG_DIR, else ~/.claude.
func ConfigDir() string { return ConfigDirWith(nil) }

// ConfigDirWith is where a Claude started by a launcher keeps its state. refs are the launcher's environment references (child variable ->
// variable of this process whose value it gets), applied the way the adapter applies them when it starts Claude: a reference to
// CLAUDE_CONFIG_DIR or HOME replaces this process's own value.
func ConfigDirWith(refs map[string]string) string {
	value := func(name string) string {
		if source, ok := refs[name]; ok {
			return os.Getenv(source)
		}
		return os.Getenv(name)
	}
	if dir := value("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home := value("HOME")
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return ""
		}
	}
	return filepath.Join(home, ".claude")
}

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]`)

// FolderName is how Claude names a working folder's transcript directory: every character that is not a letter or a digit becomes a dash.
func FolderName(cwd string) string { return nonAlphanumeric.ReplaceAllString(cwd, "-") }

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,63}$`)

// ValidID reports whether text can be a session id (it names a file, so it is never a path or a pattern).
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Result is what a scan found.
type Result struct {
	Sessions []Session
	// Unreadable counts transcripts in the folder's directories whose working folder could not be read (the first line that carries it is
	// past the part of the file that is read), which may therefore belong to the folder.
	Unreadable int
	// Truncated says there were more transcripts or sessions than one scan looks at, so Sessions is the newest part of them.
	Truncated bool
}

// ErrUnreadable is a transcript that exists but whose working folder cannot be read, so it cannot be continued from here.
var ErrUnreadable = errors.New("claude session has no readable working folder")

// Scan lists the sessions that worked in one of the folders (spellings of the same folder: as configured and resolved) or below, newest
// first. Which sessions belong is decided by the working folder each transcript records. A directory name only says where to look, and
// not even always: Claude turns every non-alphanumeric into a dash (a sibling folder can share a prefix) and shortens a long path with a
// hash (the name no longer starts like the folder's), so a directory with a long name is opened to see whose it is.
func Scan(configDir string, folders ...string) (Result, error) {
	if configDir == "" || len(folders) == 0 {
		return Result{}, errors.New("a configuration folder and an absolute project folder are required")
	}
	prefixes := make([]string, 0, len(folders))
	for i, folder := range folders {
		if !filepath.IsAbs(folder) {
			return Result{}, errors.New("a configuration folder and an absolute project folder are required")
		}
		folders[i] = filepath.Clean(folder)
		prefixes = append(prefixes, FolderName(folders[i]))
	}
	result := Result{Sessions: []Session{}}
	root := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return Result{}, err
	}
	var dirs []string
	probes := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name, dir := entry.Name(), filepath.Join(root, entry.Name())
		switch {
		case anyPrefix(name, prefixes):
			dirs = append(dirs, dir)
		case len(name) >= longDirName && probes < MaxProbes:
			probes++
			if dirHoldsFolder(dir, folders) {
				dirs = append(dirs, dir)
			}
		case len(name) >= longDirName:
			// Out of probes: this directory may be the folder's, and was not looked at.
			result.Truncated = true
		}
	}
	type candidate struct {
		path string
		info os.FileInfo
	}
	var candidates []candidate
	for _, dir := range dirs {
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if !isTranscript(file) {
				continue
			}
			info, err := file.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			candidates = append(candidates, candidate{filepath.Join(dir, file.Name()), info})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].info.ModTime().After(candidates[j].info.ModTime()) })
	active := activeSessions(configDir)
	for i, c := range candidates {
		if i >= MaxInspect {
			result.Truncated = true
			break
		}
		session, status := inspect(c.path, c.info, func(cwd string) bool { return within(cwd, folders) })
		switch status {
		case inspected:
			if len(result.Sessions) == MaxResults {
				result.Truncated = true
				return result, nil
			}
			session.Active = active[session.ID]
			result.Sessions = append(result.Sessions, session)
		case unreadable:
			result.Unreadable++
		}
	}
	return result, nil
}

func anyPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if name == prefix || strings.HasPrefix(name, prefix+"-") {
			return true
		}
	}
	return false
}

// within says cwd is one of the folders or below it.
func within(cwd string, folders []string) bool {
	for _, folder := range folders {
		if cwd == folder || strings.HasPrefix(cwd, folder+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// dirHoldsFolder looks at the newest transcript of a directory to see whether its sessions worked in one of the folders.
func dirHoldsFolder(dir string, folders []string) bool {
	files, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	var newest string
	var when time.Time
	for _, file := range files {
		if !isTranscript(file) {
			continue
		}
		if info, err := file.Info(); err == nil && info.Mode().IsRegular() && (newest == "" || info.ModTime().After(when)) {
			newest, when = filepath.Join(dir, file.Name()), info.ModTime()
		}
	}
	if newest == "" {
		return false
	}
	handle, err := os.Open(newest)
	if err != nil {
		return false
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return false
	}
	h := readHead(handle, info.Size())
	return h.cwd != "" && !h.sidechain && within(h.cwd, folders)
}

// Find looks a session up by id in every project directory.
func Find(configDir, id string) (Session, error) {
	if configDir == "" || !ValidID(id) {
		return Session{}, ErrNotFound
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", id+".jsonl"))
	if err != nil || len(matches) == 0 {
		return Session{}, ErrNotFound
	}
	sort.Strings(matches)
	active := activeSessions(configDir)
	unreadableSeen := false
	for _, path := range matches {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		session, status := inspect(path, info, func(string) bool { return true })
		switch status {
		case inspected:
			session.Active = active[session.ID]
			return session, nil
		case unreadable:
			unreadableSeen = true
		}
	}
	if unreadableSeen {
		return Session{}, ErrUnreadable
	}
	return Session{}, ErrNotFound
}

// isTranscript is a session transcript of its own: a <session id>.jsonl file. Subagent transcripts are named agent-*.jsonl.
func isTranscript(entry os.DirEntry) bool {
	name := entry.Name()
	return !entry.IsDir() && strings.HasSuffix(name, ".jsonl") && !strings.HasPrefix(name, "agent-") && ValidID(strings.TrimSuffix(name, ".jsonl"))
}

// line is the part of a transcript line that matters here.
type line struct {
	Type        string `json:"type"`
	CWD         string `json:"cwd"`
	SessionID   string `json:"sessionId"`
	GitBranch   string `json:"gitBranch"`
	Version     string `json:"version"`
	Entrypoint  string `json:"entrypoint"`
	IsSidechain bool   `json:"isSidechain"`
	AITitle     string `json:"aiTitle"`
	LastPrompt  string `json:"lastPrompt"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// head is what the lines of a transcript's start and end say.
type head struct {
	cwd, gitBranch, version, entrypoint string
	sidechain, spoke                    bool
	firstPrompt, title, last            string
}

func (h *head) consume(lines [][]byte) {
	for _, raw := range lines {
		var l line
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		if l.CWD != "" && h.cwd == "" {
			h.sidechain = l.IsSidechain
			h.cwd, h.gitBranch, h.version, h.entrypoint = l.CWD, l.GitBranch, l.Version, l.Entrypoint
		}
		switch l.Type {
		case "ai-title":
			h.title = l.AITitle
		case "last-prompt":
			h.last = l.LastPrompt
		case "user":
			h.spoke = true
			if h.firstPrompt == "" {
				h.firstPrompt = promptText(l.Message.Content)
			}
		case "assistant":
			h.spoke = true
		}
	}
}

// readHead reads the start of a transcript: the first window, and when that held no working folder (a first line longer than the window)
// a larger one once.
func readHead(file *os.File, size int64) head {
	var h head
	h.consume(readWindow(file, 0, MaxHead))
	if h.cwd == "" && size > MaxHead {
		h = head{}
		h.consume(readWindow(file, 0, min(size, MaxHeadRetry)))
	}
	return h
}

type inspection int

const (
	skipped inspection = iota
	inspected
	// unreadable is a transcript too long to say whose it is from the part that is read.
	unreadable
)

// inspect reads one transcript: its head first, and its tail only for a session the accept function takes (the working folder decides), so
// the transcripts of other folders cost half. A session that cannot be continued (a sidechain, one with nothing said in it) is skipped.
func inspect(path string, info os.FileInfo, accept func(cwd string) bool) (Session, inspection) {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	file, err := os.Open(path)
	if err != nil {
		return Session{}, skipped
	}
	defer file.Close()
	h := readHead(file, info.Size())
	if h.cwd == "" {
		if info.Size() > MaxHead {
			return Session{}, unreadable
		}
		return Session{}, skipped
	}
	if h.sidechain || !accept(h.cwd) {
		return Session{}, skipped
	}
	// A transcript that fits the head window is read once; a longer one adds its tail, where Claude appends its title and the last prompt.
	if info.Size() > MaxHead {
		h.consume(readWindow(file, info.Size()-MaxTail, MaxTail))
	}
	if !h.spoke && h.title == "" && h.last == "" {
		return Session{}, skipped
	}
	session := Session{ID: id, CWD: h.cwd, Dir: filepath.Base(filepath.Dir(path)), GitBranch: h.gitBranch, Version: h.version, Entrypoint: h.entrypoint,
		ModifiedAt: info.ModTime().UTC(), Size: info.Size()}
	switch {
	case h.title != "":
		session.Title = cleanTitle(h.title)
	case h.last != "":
		session.Title = cleanTitle(h.last)
	default:
		session.Title = cleanTitle(h.firstPrompt)
	}
	return session, inspected
}

// readWindow returns the complete lines inside [offset, offset+size) of the file. A window that starts mid-file drops its first, partial
// line; one cut off at the end drops the last. A line longer than the window is simply not seen.
func readWindow(file *os.File, offset, size int64) [][]byte {
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, size)
	n, err := file.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	buf = buf[:n]
	if offset > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			return nil
		}
	}
	atEnd := int64(n) < size
	scanner := bufio.NewScanner(bytes.NewReader(buf))
	scanner.Buffer(make([]byte, 0, 64<<10), int(size)+1)
	var lines [][]byte
	for scanner.Scan() {
		lines = append(lines, append([]byte(nil), scanner.Bytes()...))
	}
	// The last line is complete only when the window reached the end of the file or ended on a newline.
	if !atEnd && len(buf) > 0 && buf[len(buf)-1] != '\n' && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// promptText is the text of a user message: Claude writes the content as a string or as a list of typed parts.
func promptText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		for _, part := range parts {
			if part.Type == "text" && part.Text != "" {
				return part.Text
			}
		}
	}
	return ""
}

// cleanTitle makes a prompt fit one line: whitespace collapsed, control characters dropped, cut at TitleRunes.
func cleanTitle(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > TitleRunes {
		return string(runes[:TitleRunes]) + "…"
	}
	return text
}

// activeSessions is the sessions a live Claude process has registered. A registry entry whose process is gone is stale and ignored.
func activeSessions(configDir string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(configDir, "sessions"))
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(configDir, "sessions", entry.Name()))
		if err != nil || len(data) > 64<<10 {
			continue
		}
		var registered struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(data, &registered) != nil || registered.PID <= 0 || !ValidID(registered.SessionID) {
			continue
		}
		if alive(registered.PID) {
			out[registered.SessionID] = true
		}
	}
	return out
}

// alive says a process exists. EPERM means it does, under another user.
func alive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
