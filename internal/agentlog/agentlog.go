// Package agentlog keeps what each agent's harness wrote to stderr, on the machine that ran it: one rotating log per agent under
// supervisor/agent-logs/, kept for a bounded time and size, for an owner or admin to read when a turn went wrong. Beside it is the
// agent's conversation log: what the adapters parsed from the harness's event stream (the prompt, the model's text, each tool call and
// result), one record per event, with the same retention and its own rotation.
//
// The output is the harness's own and can carry whatever its tools printed, so it never leaves the machine on its own: the server asks for
// a bounded tail when a person who may see it asks. Agent keys and bearer tokens are removed before a line is written (across the
// chunks a stream arrives in), the files are 0600 in 0700 directories, and an agent's directory is named from a hash of its id, so an id
// can never name a path.
package agentlog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/logfile"
)

const (
	// FileName is an agent's current log; its rotated backups are FileName.1 to .Backups.
	FileName = "stderr.log"

	// ConversationFileName is an agent's conversation log (what the adapters parsed from the harness's event stream), rotated the same way.
	ConversationFileName = "conversation.log"

	DefaultMaxFileBytes = 1 << 20 // one file; with the backups that is 4 MiB for an agent
	// DefaultConversationFileBytes is one conversation file: a conversation is far more text than stderr, so with the backups an agent
	// may hold 16 MiB of it.
	DefaultConversationFileBytes = 4 << 20
	DefaultBackups               = 3
	DefaultMaxAge                = 14 * 24 * time.Hour // what a file's last write may be before it is removed
	DefaultMaxTotal              = 256 << 20           // all agents together; the oldest agents' files go first

	DefaultTailLines = 200
	MaxTailLines     = 2000
	MaxTailBytes     = 256 << 10 // what one read returns, whatever lines it holds

	// maxLine is how long a line may grow before it is written in pieces, and carry is how much of such a line is held back so that a
	// secret split across two pieces is still seen whole.
	maxLine = 256 << 10
	carry   = 16 << 10

	// activeWindow: a file written this recently may belong to a turn that is running, so the size cap does not remove it.
	activeWindow = 5 * time.Minute
)

// ErrNoLog says an agent has no captured output.
var ErrNoLog = errors.New("no captured output for this agent")

// ErrUnknownStream says a read named a stream this store does not keep.
var ErrUnknownStream = errors.New("unknown output stream")

// Stream names one of the two logs an agent has.
type Stream string

const (
	StreamStderr       Stream = "stderr"
	StreamConversation Stream = "conversation"
)

// ParseStream is the stream a name means; the empty name is stderr, which is what a reader that predates streams asks for.
func ParseStream(name string) (Stream, error) {
	switch Stream(name) {
	case "", StreamStderr:
		return StreamStderr, nil
	case StreamConversation:
		return StreamConversation, nil
	}
	return "", ErrUnknownStream
}

func (s Stream) fileName() string {
	if s == StreamConversation {
		return ConversationFileName
	}
	return FileName
}

var (
	unsafeName     = regexp.MustCompile(`[^a-z0-9_-]+`)
	endsWithBearer = regexp.MustCompile(`(?i)\bBearer[[:space:]]*$`)
	leadingWord    = regexp.MustCompile(`^([[:space:]]*)[^[:space:]]+`)
)

// DirName is the directory an agent's output lives in: a readable slug of its id and a hash of the whole id. The id is never part of a
// path, so nothing in it (separators, dots, an oversized name) can reach outside the store, and two ids never share a directory.
func DirName(agentID string) string {
	slug := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(agentID), "-"), "-")
	if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "-")
	}
	if slug == "" {
		slug = "agent"
	}
	sum := sha256.Sum256([]byte(agentID))
	return slug + "-" + hex.EncodeToString(sum[:6])
}

// Store is the output logs of one machine.
type Store struct {
	dir string
	now func() time.Time
	// maxFile and backups are the rotation settings (the defaults; tests make them small).
	maxFile int64
	backups int
	// maxConversation is the rotation size of a conversation file.
	maxConversation int64

	mu   sync.Mutex
	open map[string]*shared // by directory name: one writer per agent however many turns hold it
	// conversations are the open conversation logs, by directory name like open.
	conversations map[string]*shared
}

type shared struct {
	writer *logfile.Writer
	refs   int
}

// New makes a store rooted at dir (created on first use).
func New(dir string) *Store {
	return &Store{dir: dir, now: time.Now, maxFile: DefaultMaxFileBytes, backups: DefaultBackups, maxConversation: DefaultConversationFileBytes,
		open: map[string]*shared{}, conversations: map[string]*shared{}}
}

// Dir is where the logs are.
func (s *Store) Dir() string { return s.dir }

func (s *Store) agentDir(agentID string) string { return filepath.Join(s.dir, DirName(agentID)) }

// Turn is the writer for one turn's output. Everything written goes, redacted and timestamped, to the agent's log; the first line of a
// turn is a marker naming it, so output of consecutive turns can be told apart, and a turn that prints nothing leaves no trace.
type Turn struct {
	store  *Store
	key    string
	marker string

	mu     sync.Mutex
	shared *shared
	buf    []byte
	opened bool // the marker is written
	closed bool
	// afterBearer is set when a line ended with the word "Bearer": the token is the first word of the next line, which is redacted too.
	afterBearer bool
	// dropLine is set when a blob was replaced by a marker: the rest of that line is dropped, up to its newline.
	dropLine bool
	// inKey follows a PEM private key that a line began without ending it: its lines are dropped, up to and with its END line. It is
	// bounded (privateKeyLines), so a BEGIN line with no END cannot swallow the rest of a log.
	inKey adapter.PrivateKeyLines
	// held is how much of the buffer is known to hold no place to cut (see pieceEnd).
	held int
	// scanned is how much of the buffer has been searched for a newline.
	scanned int
}

// OpenTurn starts capturing one turn of an agent.
func (s *Store) OpenTurn(agentID, turnID string, generation int, mode string) (*Turn, error) {
	if agentID == "" {
		return nil, errors.New("agent id is empty")
	}
	key := DirName(agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.open[key]
	if entry == nil {
		dir := filepath.Join(s.dir, key)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		writer, err := logfile.Open(filepath.Join(dir, FileName), s.maxFile, s.backups)
		if err != nil {
			return nil, err
		}
		entry = &shared{writer: writer}
		s.open[key] = entry
	}
	entry.refs++
	return &Turn{store: s, key: key, shared: entry, inKey: adapter.PrivateKeyLines{Limit: privateKeyLines}, marker: fmt.Sprintf("=== turn %s gen %d %s ===", cleanToken(turnID), generation, cleanToken(mode))}, nil
}

// cleanToken keeps a value in a marker line from carrying a line break or control character into the log.
func cleanToken(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, value)
}

// Write takes the harness's stderr as it arrives. It never fails the writer it is tee'd from.
func (t *Turn) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return len(p), nil
	}
	written := len(p)
	if t.dropLine {
		// The rest of a line that was replaced by a marker is dropped, up to its end.
		end := indexByte(p, '\n')
		if end < 0 {
			return written, nil
		}
		p, t.dropLine = p[end+1:], false
	}
	t.buf = append(t.buf, p...)
	for {
		// Only what arrived since the last look is searched for the end of the line.
		newline := indexByte(t.buf[t.scanned:], '\n')
		if newline < 0 {
			t.scanned = len(t.buf)
			break
		}
		newline += t.scanned
		if line := t.buf[:newline]; isBlob(line) {
			t.emitMarker(longToken) // a blob that ends in the write that reaches the limit is replaced like one that is held
		} else {
			t.emit(string(line))
		}
		t.buf = t.buf[newline+1:]
		t.held, t.scanned = 0, 0
	}
	// A very long line is written in pieces, and only ever cut at a space or tab (a secret has none, except after the word Bearer, whose
	// token is the next run): so no secret, however it is made, is ever cut in two, and no byte of a multi-byte character either. A line
	// with no space in it is held until it ends; one that reaches hardLine with no space at all is a blob nobody reads in a debug log, so
	// it is replaced by a marker and the rest of that line is dropped, up to its end (whatever is left of a cut token must not be taken for
	// the start of anything else).
	for len(t.buf) >= maxLine {
		cut := pieceEnd(t.buf, len(t.buf)-carry, t.held)
		if cut > 0 {
			t.emit(string(t.buf[:cut]))
			t.buf = t.buf[cut:]
			t.held, t.scanned = 0, len(t.buf) // the rest has no newline: that was searched for already
			continue
		}
		// Nothing in the buffer is a place to cut, and that stays true as more arrives: it is not searched again.
		t.held = len(t.buf)
		if !isBlob(t.buf) {
			break
		}
		t.emitMarker(longToken)
		t.buf, t.held, t.scanned, t.dropLine = nil, 0, 0, true
		break
	}
	return written, nil
}

// privateKeyLines is how many lines after a private key's BEGIN line are taken for the key when no END line comes.
const privateKeyLines = 400

const (
	// hardLine is how long a line with no space in it may grow before it is replaced.
	hardLine  = 4 * maxLine
	longToken = "[long token redacted; rest of line dropped]"
)

// isBlob says whether a line is one nobody reads in a debug log: as long as hardLine and with no place to cut it. It is the one test, for
// a line that is still being held and for one that has just ended.
func isBlob(line []byte) bool {
	return len(line) >= hardLine && pieceEnd(line, len(line)-carry, 0) == 0
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' }

// pieceEnd is where to cut a long line at or before limit: just after the nearest earlier space or tab that does not follow the word
// Bearer; failing that just after the nearest later one; 0 when the buffer holds no place to cut. The first `searched` bytes are known to
// hold none (an earlier call found none in them), so they are not looked at again.
func pieceEnd(buf []byte, limit, searched int) int {
	for i := limit; i > 0 && i >= searched; i-- {
		if isSpace(buf[i]) && !followsBearer(buf[:i]) {
			return i + 1
		}
	}
	start := limit + 1
	if searched > start {
		start = searched
	}
	for i := start; i < len(buf); i++ {
		if isSpace(buf[i]) && !followsBearer(buf[:i]) {
			return i + 1
		}
	}
	return 0
}

// followsBearer says whether the last word of b is "Bearer" (in any case): the space after it is part of a credential.
func followsBearer(b []byte) bool {
	end := len(b)
	for end > 0 && (b[end-1] == ' ' || b[end-1] == '\t') {
		end--
	}
	const word = "bearer"
	if end < len(word) || !strings.EqualFold(string(b[end-len(word):end]), word) {
		return false
	}
	before := end - len(word)
	return before == 0 || !isWordByte(b[before-1])
}

func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// emit writes one redacted, timestamped line, after the turn's marker if this is the turn's first.
func (t *Turn) emit(line string) {
	if t.inKey.Skip(line) {
		// A line of a private key whose BEGIN line was replaced by a marker: it is dropped, and so is the END line that closes it.
		return
	}
	stamp := t.store.now().UTC().Format("2006-01-02T15:04:05Z") + " "
	var out strings.Builder
	if !t.opened {
		t.opened = true
		out.WriteString(stamp + t.marker + "\n")
	}
	line = strings.TrimRight(line, "\r")
	if t.afterBearer {
		line = leadingWord.ReplaceAllString(line, "$1[redacted]")
	}
	t.afterBearer = endsWithBearer.MatchString(line)
	out.WriteString(stamp + adapter.RedactSecrets(line) + "\n")
	_, _ = t.shared.writer.Write([]byte(out.String()))
}

// emitMarker writes a line of the log's own (not the harness's): it is not redacted, and it is outside the state of the harness's text,
// so the line after it starts clean.
func (t *Turn) emitMarker(text string) {
	stamp := t.store.now().UTC().Format("2006-01-02T15:04:05Z") + " "
	var out strings.Builder
	if !t.opened {
		t.opened = true
		out.WriteString(stamp + t.marker + "\n")
	}
	out.WriteString(stamp + text + "\n")
	t.afterBearer = false
	_, _ = t.shared.writer.Write([]byte(out.String()))
}

// Close writes what is left of a last line without a newline and lets go of the file.
func (t *Turn) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if len(t.buf) > 0 {
		t.emit(string(t.buf))
		t.buf = nil
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	t.shared.refs--
	if t.shared.refs <= 0 {
		_ = t.shared.writer.Close()
		delete(t.store.open, t.key)
	}
	return nil
}

// CutMarker is the first line of a conversation tail that begins inside a record: the record is longer than one read may be, so its
// first line, which says what it is, is not in the tail.
const CutMarker = "... earlier part cut"

// TurnStartPrefix is how the text of a turn_start record begins; the turn's id follows it.
const TurnStartPrefix = "turn "

// TurnOfStart is the turn id a turn_start record's text names, or "".
func TurnOfStart(text string) string {
	rest, ok := strings.CutPrefix(text, TurnStartPrefix)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, " ")
	return id
}

// turnBefore is the id of the last turn that starts in the first n bytes of a conversation log's text: the turn the record at n is in.
func turnBefore(log string, n int) string {
	if n <= 0 || n > len(log) {
		return ""
	}
	marker := " " + string(adapter.ConversationTurnStart) + " "
	for before := log[:n]; ; {
		at := strings.LastIndex(before, marker)
		if at < 0 {
			return ""
		}
		line := strings.LastIndexByte(before[:at], '\n') + 1
		// Only a record's own first line counts: the same words in a tool's output are on a line that starts with a tab.
		if !strings.HasPrefix(before[line:], continuation) && !strings.ContainsAny(before[line:at], " \t") {
			end := strings.IndexByte(log[at:], '\n')
			if end < 0 {
				end = len(log) - at
			}
			return TurnOfStart(log[at+len(marker) : at+end])
		}
		before = before[:line]
	}
}

// continuation starts every line of a record after its first, so a line that begins with a time is always a record's own.
const continuation = "\t"

// Conversation is the writer for one turn's conversation records. Each record is a line `<UTC time> <kind> <first line of text>`, and
// the rest of its text follows on lines that start with a tab. It writes what it is given: masking secrets and bounding a record are
// done by adapter.Conversation, which is the only thing that feeds it.
type Conversation struct {
	store *Store
	key   string

	mu     sync.Mutex
	shared *shared
	closed bool
}

// OpenConversation starts capturing the conversation of one turn of an agent.
func (s *Store) OpenConversation(agentID string) (*Conversation, error) {
	if agentID == "" {
		return nil, errors.New("agent id is empty")
	}
	key := DirName(agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.conversations[key]
	if entry == nil {
		dir := filepath.Join(s.dir, key)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		writer, err := logfile.Open(filepath.Join(dir, ConversationFileName), s.maxConversation, s.backups)
		if err != nil {
			return nil, err
		}
		entry = &shared{writer: writer}
		s.conversations[key] = entry
	}
	entry.refs++
	return &Conversation{store: s, key: key, shared: entry}, nil
}

// Record writes one record. It never fails the turn it is fed from.
func (c *Conversation) Record(kind adapter.ConversationKind, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	label := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(string(kind)), "_"), "_")
	if label == "" {
		label = "record"
	}
	text = strings.ToValidUTF8(strings.ReplaceAll(strings.TrimRight(text, "\r\n"), "\r\n", "\n"), "�")
	first, rest, more := strings.Cut(text, "\n")
	var out strings.Builder
	out.Grow(len(text) + 64)
	out.WriteString(c.store.now().UTC().Format("2006-01-02T15:04:05Z") + " " + label)
	if first = cleanLine(first); first != "" {
		out.WriteString(" " + first)
	}
	out.WriteByte('\n')
	if more {
		for _, line := range strings.Split(rest, "\n") {
			out.WriteString(continuation + cleanLine(line) + "\n")
		}
	}
	_, _ = c.shared.writer.Write([]byte(out.String()))
}

// cleanLine keeps a line of a record from carrying a control character (a carriage return, an escape sequence's start) into the log; a
// tab stays.
func cleanLine(line string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, line)
}

// Close lets go of the file.
func (c *Conversation) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	c.shared.refs--
	if c.shared.refs <= 0 {
		_ = c.shared.writer.Close()
		delete(c.store.conversations, c.key)
	}
	return nil
}

// Page is a tail of an agent's output.
type Page struct {
	Text      string    `json:"text"`
	Bytes     int64     `json:"bytes"`               // what the agent's logs hold in all
	Truncated bool      `json:"truncated,omitempty"` // earlier output exists that this page does not show
	Files     int       `json:"files"`
	UpdatedAt time.Time `json:"updated_at"`
	// Turn is the id of the turn the page's first records belong to, when that turn's start is before the page (a conversation only):
	// whoever decides what of a turn may be shown needs to know the turn of every record. Empty when the start is not in what was read.
	Turn string `json:"turn,omitempty"`
}

// Tail returns the last lines of an agent's output: at most lines of them (DefaultTailLines when 0, never more than MaxTailLines) and at
// most maxBytes (MaxTailBytes when 0, never more), cut at a line start. ErrNoLog when the agent has none.
func (s *Store) Tail(agentID string, lines, maxBytes int) (Page, error) {
	return s.TailStream(agentID, StreamStderr, lines, maxBytes)
}

// TailStream is Tail for one of an agent's two logs. A conversation tail starts at a record's first line, so it may hold more lines than
// were asked for (never more bytes); only when one record is longer than a read may be does it start with CutMarker and the end of that
// record.
func (s *Store) TailStream(agentID string, stream Stream, lines, maxBytes int) (Page, error) {
	if lines <= 0 {
		lines = DefaultTailLines
	}
	if lines > MaxTailLines {
		lines = MaxTailLines
	}
	if maxBytes <= 0 || maxBytes > MaxTailBytes {
		maxBytes = MaxTailBytes
	}
	dir := s.agentDir(agentID)
	names := logfile.Names(filepath.Join(dir, stream.fileName()), s.backups)
	var page Page
	var parts []string // newest file first
	for _, name := range names {
		info, err := os.Stat(name)
		if err != nil {
			continue
		}
		if page.Files == 0 {
			page.UpdatedAt = info.ModTime().UTC()
		}
		page.Files++
		page.Bytes += info.Size()
		// Reading stops once enough is held: older files only matter when the newer ones are short.
		if enough(parts, lines, maxBytes) {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		parts = append(parts, string(data))
	}
	if page.Files == 0 {
		return Page{}, ErrNoLog
	}
	// Oldest first, so the lines come out in order.
	var all []string
	for i := len(parts) - 1; i >= 0; i-- {
		all = append(all, strings.SplitAfter(parts[i], "\n")...)
	}
	if n := len(all); n > 0 && all[n-1] == "" {
		all = all[:n-1]
	}
	whole := ""
	if stream == StreamConversation {
		whole = strings.Join(all, "")
	}
	if len(all) > lines {
		start := len(all) - lines
		// A conversation tail begins at a record: when the cut falls inside one, the tail is extended back to that record's first line (the
		// byte limit below still bounds it).
		for stream == StreamConversation && start > 0 && strings.HasPrefix(all[start], continuation) {
			start--
		}
		if start > 0 {
			all = all[start:]
			page.Truncated = true
		}
	}
	text := strings.Join(all, "")
	if len(text) > maxBytes {
		text = text[len(text)-maxBytes:]
		if cut := strings.IndexByte(text, '\n'); cut >= 0 && cut+1 < len(text) {
			text = text[cut+1:]
		}
		page.Truncated = true
	}
	tail := text
	if stream == StreamConversation && strings.HasPrefix(text, continuation) {
		// One record is longer than a read may be: what fits of its end is kept, behind a line that says its beginning is not here. A read
		// never comes back empty while the log has content.
		text = CutMarker + "\n" + text
	}
	if stream == StreamConversation {
		page.Turn = turnBefore(whole, len(whole)-len(tail))
	}
	page.Text = strings.ToValidUTF8(text, "�")
	if int64(len(page.Text)) < page.Bytes {
		page.Truncated = true
	}
	return page, nil
}

// enough says whether the parts already read (newest first) hold what a tail needs.
func enough(parts []string, lines, maxBytes int) bool {
	total, count := 0, 0
	for _, part := range parts {
		total += len(part)
		count += strings.Count(part, "\n")
	}
	return count > lines || total > maxBytes
}

// Size is what a machine reports about its output logs for the Health disk report: the bytes they hold and how many agents have any.
type Size struct {
	Bytes  int64 `json:"bytes"`
	Agents int   `json:"agents"`
}

// Usage is what the store holds: its bytes and how many agents have output.
func (s *Store) Usage() (bytes int64, agents int) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		found := false
		files, _ := os.ReadDir(filepath.Join(s.dir, entry.Name()))
		for _, file := range files {
			if info, err := file.Info(); err == nil && info.Mode().IsRegular() {
				bytes += info.Size()
				found = true
			}
		}
		if found {
			agents++
		}
	}
	return bytes, agents
}

// SweepResult is what a sweep removed.
type SweepResult struct {
	Files int
	Bytes int64
}

// Sweep removes output older than maxAge by its last write, then, if the logs still hold more than maxTotal bytes, the oldest files
// first (never one that was written in the last few minutes, or of an agent whose turn is capturing now). Empty agent directories go.
func (s *Store) Sweep(maxAge time.Duration, maxTotal int64) (SweepResult, error) {
	var result SweepResult
	now := s.now()
	type item struct {
		path string
		size int64
		mod  time.Time
		dir  string
	}
	var items []item
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, err
	}
	s.mu.Lock()
	active := make(map[string]bool, len(s.open)+len(s.conversations))
	for key := range s.open {
		active[key] = true
	}
	for key := range s.conversations {
		active[key] = true
	}
	s.mu.Unlock()
	var total int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(s.dir, entry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, file := range files {
			info, err := file.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			path := filepath.Join(dir, file.Name())
			if maxAge > 0 && now.Sub(info.ModTime()) > maxAge && !active[entry.Name()] {
				if err := os.Remove(path); err == nil {
					result.Files++
					result.Bytes += info.Size()
				}
				continue
			}
			total += info.Size()
			items = append(items, item{path: path, size: info.Size(), mod: info.ModTime(), dir: entry.Name()})
		}
	}
	if maxTotal > 0 && total > maxTotal {
		sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
		for _, it := range items {
			if total <= maxTotal {
				break
			}
			if active[it.dir] || now.Sub(it.mod) < activeWindow {
				continue
			}
			if err := os.Remove(it.path); err == nil {
				total -= it.size
				result.Files++
				result.Bytes += it.size
			}
		}
	}
	// Directories left with nothing in them.
	for _, entry := range entries {
		if entry.IsDir() && !active[entry.Name()] {
			_ = os.Remove(filepath.Join(s.dir, entry.Name())) // only an empty one goes
		}
	}
	return result, nil
}
