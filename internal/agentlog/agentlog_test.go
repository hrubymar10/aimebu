package agentlog

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hrubymar10/aimebu/v2/internal/logfile"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestMain(m *testing.M) {
	_, cleanup := testenv.Isolate()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func newStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	store := New(filepath.Join(t.TempDir(), "agent-logs"))
	clock := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	return store, &clock
}

func write(t *testing.T, store *Store, agent, turn, text string) {
	t.Helper()
	log, err := store.OpenTurn(agent, turn, 1, "normal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

// An agent id never reaches a path: whatever it holds, the directory is a slug and a hash.
func TestDirNameIsSafeForAnyAgentID(t *testing.T) {
	safe := regexp.MustCompile(`^[a-z0-9_-]+-[0-9a-f]{12}$`)
	ids := []string{"agt_1", "../../etc/passwd", "a/b/c", `..\..\windows`, "/absolute", "with space", "tab\tand\nnewline", "nul\x00byte", "ünïcødé-名前", "", ".", "..", strings.Repeat("long", 500), "UPPER", "agt_x@project"}
	seen := map[string]string{}
	for _, id := range ids {
		name := DirName(id)
		if !safe.MatchString(name) || len(name) > 32+1+12 {
			t.Errorf("DirName(%q) = %q is not a plain directory name", id, name)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("%q and %q share the directory %q", id, other, name)
		}
		seen[name] = id
		if DirName(id) != name {
			t.Errorf("DirName(%q) is not stable", id)
		}
	}
	// Ids that differ only where the slug would not tell them apart still get their own directory.
	if DirName("Agt_1") == DirName("agt_1") || DirName("a/b") == DirName("a-b") {
		t.Fatal("ids that slug alike share a directory")
	}
}

func TestHostileAgentIDsStayInsideTheStore(t *testing.T) {
	store, _ := newStore(t)
	for _, id := range []string{"../../escape", "/tmp/escape", "a/../../b", "..", "."} {
		write(t, store, id, "t1", "hello\n")
		if _, err := store.Tail(id, 10, 0); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}
	root, err := filepath.Abs(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	filepath.Walk(filepath.Dir(root), func(path string, info os.FileInfo, err error) error {
		if err == nil && !strings.HasPrefix(path, filepath.Dir(root)) {
			t.Errorf("%s is outside the store", path)
		}
		return nil
	})
	entries, _ := os.ReadDir(filepath.Dir(root))
	if len(entries) != 1 || entries[0].Name() != filepath.Base(root) {
		t.Fatalf("something was written beside the store: %v", entries)
	}
}

func TestTurnWritesStampedLinesAfterOneMarkerAndKeepsAPartialLastLine(t *testing.T) {
	store, clock := newStore(t)
	log, err := store.OpenTurn("agt_1", "turn-7", 3, "correction")
	if err != nil {
		t.Fatal(err)
	}
	log.Write([]byte("first\nsecond"))
	*clock = clock.Add(time.Minute)
	log.Write([]byte(" half\nthird\r\nno newline at the end"))
	log.Close()
	page, err := store.Tail("agt_1", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-10-22T12:00:00Z === turn turn-7 gen 3 correction ===\n" +
		"2026-10-22T12:00:00Z first\n" +
		"2026-10-22T12:01:00Z second half\n" +
		"2026-10-22T12:01:00Z third\n" +
		"2026-10-22T12:01:00Z no newline at the end\n"
	if page.Text != want || page.Files != 1 || page.Truncated {
		t.Fatalf("tail = %q (files %d truncated %v)\nwant %q", page.Text, page.Files, page.Truncated, want)
	}
}

func TestAMarkerNamesEachTurnAndAControlCharacterCannotForgeOne(t *testing.T) {
	store, _ := newStore(t)
	write(t, store, "agt_1", "turn-a", "from a\n")
	write(t, store, "agt_1", "turn-b\n2026-10-22T12:00:00Z fake line", "from b\n")
	page, _ := store.Tail("agt_1", 100, 0)
	if strings.Count(page.Text, "=== turn ") != 2 || strings.Contains(page.Text, "\n2026-10-22T12:00:00Z fake line") {
		t.Fatalf("markers:\n%s", page.Text)
	}
}

func TestSecretsAreRedactedWhateverWayTheStreamIsCut(t *testing.T) {
	store, _ := newStore(t)
	text := "start aak_abc123-DEF_456 end\nAuthorization: Bearer eyJhbGciOi.payload.sig tail\nBearer\nnexttoken-on-the-next-line and more\nclean line\n"
	// Written whole, in two halves, and a byte at a time: the same file content.
	var contents []string
	for _, pieces := range []int{1, 2, len(text)} {
		agent := fmt.Sprintf("agt_%d", pieces)
		log, err := store.OpenTurn(agent, "t", 1, "normal")
		if err != nil {
			t.Fatal(err)
		}
		step := (len(text) + pieces - 1) / pieces
		for i := 0; i < len(text); i += step {
			end := i + step
			if end > len(text) {
				end = len(text)
			}
			log.Write([]byte(text[i:end]))
		}
		log.Close()
		page, _ := store.Tail(agent, 100, 0)
		for _, secret := range []string{"aak_abc123", "eyJhbGciOi", "nexttoken-on-the-next-line"} {
			if strings.Contains(page.Text, secret) {
				t.Errorf("%d pieces: %q survived:\n%s", pieces, secret, page.Text)
			}
		}
		for _, kept := range []string{"start [redacted] end", "Bearer [redacted] tail", "[redacted] and more", "clean line"} {
			if !strings.Contains(page.Text, kept) {
				t.Errorf("%d pieces: %q is gone:\n%s", pieces, kept, page.Text)
			}
		}
		contents = append(contents, regexp.MustCompile(`\d{4}-\d\d-\d\dT[\d:]+Z `).ReplaceAllString(page.Text, ""))
	}
	if contents[0] != contents[1] || contents[1] != contents[2] {
		t.Fatalf("the cut of the stream changed the log:\n%q\n%q\n%q", contents[0], contents[1], contents[2])
	}
}

// A line longer than a piece is written in pieces, only ever cut at a space, so no secret is cut in two whatever it is made of and
// wherever the cut would have fallen; a line with no space in it is held until it ends, and a blob too long to hold is replaced by a marker.
func TestAVeryLongLineIsSplitWithoutSplittingASecret(t *testing.T) {
	store, _ := newStore(t)
	// Written in 70000-byte chunks the first cut would be at 280000-carry: put each secret across that point, at every offset of its
	// prefix and a little beyond.
	cutAt := 4*70000 - carry
	secret := "aak_SECRETSECRETSECRET"
	cases := map[string]string{}
	for offset := 0; offset <= len("aak_")+2; offset++ {
		cases[fmt.Sprintf("a key with its start %d bytes before the cut", offset)] = strings.Repeat("x", cutAt-offset-1) + " " + secret + " " + strings.Repeat("y ", 20000) + "\n"
		cases[fmt.Sprintf("a key %d bytes before the cut, no spaces", offset)] = strings.Repeat("x", cutAt-offset) + secret + strings.Repeat("y", 40000) + "\n"
	}
	cases["a bearer token with punctuation across the cut"] = strings.Repeat("x", cutAt-12) + " Bearer SECRETSECRETSECRET./+=SECRETSECRET " + strings.Repeat("y ", 20000) + "\n"
	cases["a bearer token and a key together"] = strings.Repeat("x ", (cutAt-30)/2) + "Bearer " + secret + "./+=tail " + strings.Repeat("y ", 20000) + "\n"
	cases["a blob too long to hold, with a key in it"] = strings.Repeat("z", 15*70000-carry-10) + secret + strings.Repeat("y", 20000) + "\n"
	cases["a blob too long to hold"] = strings.Repeat("z", hardLine+2000) + "\n"
	for name, line := range cases {
		agent := "agt_" + strings.NewReplacer(" ", "_", ",", "").Replace(name)
		log, err := store.OpenTurn(agent, "t", 1, "normal")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(line); i += 70000 {
			end := i + 70000
			if end > len(line) {
				end = len(line)
			}
			log.Write([]byte(line[i:end]))
		}
		log.Close()
		// A line longer than a file rotates the files as it is written, so everything the agent has is read, rotated files included.
		var data []byte
		for _, file := range logfile.Names(filepath.Join(store.dir, DirName(agent), FileName), DefaultBackups) {
			if part, err := os.ReadFile(file); err == nil {
				data = append(data, part...)
			}
		}
		if len(data) == 0 {
			t.Fatalf("%s: nothing was written", name)
		}
		if strings.Contains(string(data), "SECRETSECRET") || strings.Contains(string(data), "aak_") || strings.Contains(string(data), "./+=") {
			t.Errorf("%s: the secret reached the file", name)
		}
		if !utf8.Valid(data) {
			t.Errorf("%s: the log is not valid UTF-8", name)
		}
		if strings.HasPrefix(name, "a blob too long") {
			if !strings.Contains(string(data), longToken) || strings.Count(string(data), "z") != 0 {
				t.Errorf("%s: the blob was not replaced (%d bytes, %d z)", name, len(data), strings.Count(string(data), "z"))
			}
			if strings.Contains(string(data), "yyyy") {
				t.Errorf("%s: the rest of the blob was not dropped", name)
			}
		} else if strings.HasPrefix(name, "a key") && strings.Contains(name, "no spaces") {
			// A line with no space in it ends at its newline: held, then written whole, redacted.
			if !strings.Contains(string(data), "[redacted]") {
				t.Errorf("%s: no redaction in the file", name)
			}
		}
	}
}

func readAll(t *testing.T, store *Store, agent string) string {
	t.Helper()
	names := logfile.Names(filepath.Join(store.dir, DirName(agent), FileName), DefaultBackups)
	var data []byte
	for i := len(names) - 1; i >= 0; i-- { // oldest first
		if part, err := os.ReadFile(names[i]); err == nil {
			data = append(data, part...)
		}
	}
	return string(data)
}

// A blob replaced by a marker takes the rest of its line with it: a token cut by the replacement, a Bearer at its end and the word after
// it must not become the start of anything else; and the marker itself and the next line start clean.
func TestAReplacedBlobDropsTheRestOfItsLineAndLeavesNoState(t *testing.T) {
	store, _ := newStore(t)
	blob := strings.Repeat("z", hardLine+10)
	write := func(agent string, parts ...string) string {
		log, err := store.OpenTurn(agent, "t", 1, "normal")
		if err != nil {
			t.Fatal(err)
		}
		for _, part := range parts {
			for i := 0; i < len(part); i += 70000 {
				end := i + 70000
				if end > len(part) {
					end = len(part)
				}
				log.Write([]byte(part[i:end]))
			}
		}
		log.Close()
		return readAll(t, store, agent)
	}
	for name, parts := range map[string][]string{
		"Bearer ends the blob, its token is the next write":  {blob + "Bearer", " s3cr3t more\n"},
		"the blob is followed by a Bearer in the same write": {blob + " Bearer s3cr3t\n"},
		"the blob is followed by text, then a Bearer line":   {blob + " words\n", "Bearer s3cr3t\n"},
		"a key follows in a later write":                     {blob, " aak_SECRETSECRET and more\n"},
	} {
		got := write("agt_"+strings.ReplaceAll(name, " ", "_"), append(parts, "after\n")...)
		if strings.Contains(got, "s3cr3t") || strings.Contains(got, "SECRETSECRET") {
			t.Errorf("%s: a secret reached the log", name)
		}
		// A blob with a space after it in the same line is a long run cut at that space, and may be written; one with no space in its
		// line is replaced, and the rest of the line goes with it.
		replaced := !strings.Contains(name, "followed by")
		if replaced && (!strings.Contains(got, longToken) || strings.Contains(got, "z")) {
			t.Errorf("%s: the blob was not replaced:\n%.300s", name, got)
		}
		if !strings.HasSuffix(strings.TrimSpace(got), " after") {
			t.Errorf("%s: the next line is missing:\n%.300s", name, got)
		}
	}
	// The line after a replaced blob is read afresh, even one that is itself a Bearer line: its token is redacted as usual.
	got := write("agt_clean_state", blob+"\n", "Authorization: Bearer s3cr3t\n")
	if strings.Contains(got, "s3cr3t") || !strings.Contains(got, "Authorization: Bearer [redacted]") {
		t.Errorf("state leaked across the replacement:\n%.300s", got)
	}
	// Two blobs in a row are each replaced.
	got = write("agt_two_blobs", blob+"\n", blob+"\n", "end\n")
	if strings.Count(got, longToken) != 2 || strings.Contains(got, "z") || !strings.HasSuffix(strings.TrimSpace(got), " end") {
		t.Errorf("two blobs:\n%.300s", got)
	}
}

// Taking a long line in tiny writes costs a look at each new byte, not at the whole buffer again for every write: the search for a place
// to cut and the search for the end of the line both resume where they stopped.
//
// The check is about how the time grows, not how long it takes, so it holds on a slow or loaded machine: each case is timed at a size S and at
// 4S, the best of three runs each. Linear work takes about 4 times as long at 4S, quadratic work about 16 times; the test fails above 10.
// (A time under scalingFloor at S is taken as scalingFloor, since the ratio of two tiny timings is noise.) A far looser absolute cap only
// stops a hang.
func TestHoldingALongLineIsNotQuadratic(t *testing.T) {
	const (
		scalingFloor = 20 * time.Millisecond
		hangGuard    = 60 * time.Second
		runs         = 3
	)
	timeIt := func(step, size int, words bool, ending string) time.Duration {
		best := time.Duration(1<<63 - 1)
		for run := 0; run < runs; run++ {
			store, _ := newStore(t)
			log, err := store.OpenTurn("agt_slow", "t", 1, "normal")
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Repeat("z", size)
			if words {
				text = strings.Repeat("word ", size/5)
			}
			text += ending
			started := time.Now()
			for i := 0; i < len(text); i += step {
				end := i + step
				if end > len(text) {
					end = len(text)
				}
				log.Write([]byte(text[i:end]))
			}
			log.Close()
			if took := time.Since(started); took < best {
				best = took
			}
		}
		return best
	}
	for name, tc := range map[string]struct {
		step, size int // size is the larger of the two sizes timed (4S)
		words      bool
		ending     string
	}{
		"1-byte writes, a line that ends": {1, 80000, false, "\n"},
		"64-byte writes, a blob held":     {64, maxLine + maxLine/2, false, ""},
		"64-byte writes, then a newline":  {64, maxLine + maxLine/2, false, "\n"},
		"1-byte writes of words":          {1, 80000, true, "\n"},
	} {
		small := timeIt(tc.step, tc.size/4, tc.words, tc.ending)
		large := timeIt(tc.step, tc.size, tc.words, tc.ending)
		if large > hangGuard {
			t.Errorf("%s: %d bytes in %d-byte writes took %s", name, tc.size, tc.step, large)
		}
		t.Logf("%s: %s at %d bytes, %s at %d", name, small, tc.size/4, large, tc.size)
		base := small
		if base < scalingFloor {
			base = scalingFloor
		}
		if large > 10*base {
			t.Errorf("%s: %d bytes took %s, %d bytes took %s (%.1fx for 4x the bytes: more than linear growth)", name, tc.size/4, small, tc.size, large, float64(large)/float64(small))
		}
	}
}

// A blob is replaced by the same rule whether its line is still being held or has just ended, with or without spaces that cannot cut it.
func TestABlobIsTheSameKindOfLineHeldOrEnded(t *testing.T) {
	cases := map[string]string{
		"no space":                          strings.Repeat("z", hardLine+5),
		"spaces only after the word Bearer": "Bearer " + strings.Repeat("z", hardLine),
	}
	for name, line := range cases {
		if !isBlob([]byte(line)) {
			t.Errorf("%s: not a blob", name)
		}
	}
	if isBlob([]byte(strings.Repeat("z", hardLine+5) + " words")) {
		t.Error("a line with a place to cut was taken for a blob")
	}
	if isBlob([]byte(strings.Repeat("z", hardLine-5))) {
		t.Error("a line shorter than the limit was taken for a blob")
	}
}

// Multi-byte text is never cut in the middle of a character: cuts are at spaces only, and a blob is replaced whole.
func TestAVeryLongLineOfMultibyteTextStaysValid(t *testing.T) {
	store, _ := newStore(t)
	for name, line := range map[string]string{
		"words":                      strings.Repeat("héllo 日本語 ", 40000) + "\n",
		"no spaces, below the limit": strings.Repeat("日本語", 90000) + "\n",
		"no spaces, a blob":          strings.Repeat("日本語é", hardLine/3) + "\n",
	} {
		agent := "agt_" + strings.ReplaceAll(name, " ", "_")
		log, _ := store.OpenTurn(agent, "t", 1, "normal")
		for i := 0; i < len(line); i += 70001 { // an odd step, so chunk edges fall inside characters
			end := i + 70001
			if end > len(line) {
				end = len(line)
			}
			log.Write([]byte(line[i:end]))
		}
		log.Close()
		var data []byte
		for _, file := range logfile.Names(filepath.Join(store.dir, DirName(agent), FileName), DefaultBackups) {
			if part, err := os.ReadFile(file); err == nil {
				data = append(data, part...)
			}
		}
		if len(data) == 0 || !utf8.Valid(data) || strings.Contains(string(data), "\ufffd") {
			t.Errorf("%s: %d bytes, valid %v", name, len(data), utf8.Valid(data))
		}
	}
}

func TestFollowsBearer(t *testing.T) {
	for text, want := range map[string]bool{"Bearer": true, "x Bearer ": true, "bearer\t": true, "BEARER   ": true, "a Bearer": true, "xBearer": false, "Bearers": false, "bear": false, "": false, "no": false, "Authorization: Bearer": true} {
		if got := followsBearer([]byte(text)); got != want {
			t.Errorf("followsBearer(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestFilesArePrivateAndOutputRotates(t *testing.T) {
	store, _ := newStore(t)
	store.maxFile, store.backups = 400, 2
	for i := 0; i < 12; i++ {
		write(t, store, "agt_1", fmt.Sprintf("turn-%02d", i), strings.Repeat("a long line of harness output ", 3)+"\n")
	}
	dir := filepath.Join(store.dir, DirName("agt_1"))
	names := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		names[entry.Name()] = true
		info, _ := entry.Info()
		if info.Mode().Perm() != 0600 {
			t.Errorf("%s is %v", entry.Name(), info.Mode().Perm())
		}
	}
	if !names["stderr.log"] || !names["stderr.log.1"] || !names["stderr.log.2"] || names["stderr.log.3"] {
		t.Fatalf("files: %v", names)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0700 {
		t.Fatalf("directory is %v", info.Mode().Perm())
	}
	page, err := store.Tail("agt_1", 1000, 0)
	if err != nil || page.Files != 3 || !strings.Contains(page.Text, "turn-11") || strings.Contains(page.Text, "turn-00") {
		t.Fatalf("tail after rotation: files %d truncated %v err %v\n%s", page.Files, page.Truncated, err, page.Text)
	}
}

func TestTailLimitsLinesAndBytesAndReportsWhatIsMissing(t *testing.T) {
	store, _ := newStore(t)
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&b, "line %02d\n", i)
	}
	write(t, store, "agt_1", "t", b.String())
	page, err := store.Tail("agt_1", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(page.Text), "\n"); len(lines) != 3 || !strings.HasSuffix(lines[2], "line 50") || !page.Truncated {
		t.Fatalf("3 lines: %q truncated %v", page.Text, page.Truncated)
	}
	page, _ = store.Tail("agt_1", 0, 100) // by bytes: a whole line or nothing at the cut
	if len(page.Text) > 100 || !strings.HasPrefix(page.Text, "2026-") || !page.Truncated {
		t.Fatalf("100 bytes: %q", page.Text)
	}
	page, _ = store.Tail("agt_1", 100000, 0) // more than the limit is the limit
	if strings.Count(page.Text, "\n") != 51 || page.Truncated {
		t.Fatalf("everything: %d lines truncated %v", strings.Count(page.Text, "\n"), page.Truncated)
	}
	page, _ = store.Tail("agt_1", 0, 0)
	if page.Bytes <= 0 || page.UpdatedAt.IsZero() {
		t.Fatalf("page facts: %+v", page)
	}
	if _, err := store.Tail("agt_never", 10, 0); err != ErrNoLog {
		t.Fatalf("an agent with no output: %v", err)
	}
	if _, err := store.Tail("../../etc/passwd", 10, 0); err != ErrNoLog {
		t.Fatalf("a hostile id found something: %v", err)
	}
}

func TestTurnsOfOneAgentShareTheFileAndLoseNothingWhenConcurrent(t *testing.T) {
	store, _ := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log, err := store.OpenTurn("agt_1", fmt.Sprintf("turn-%d", i), 1, "normal")
			if err != nil {
				t.Error(err)
				return
			}
			for j := 0; j < 50; j++ {
				fmt.Fprintf(log, "turn %d line %d\n", i, j)
			}
			log.Close()
		}()
	}
	wg.Wait()
	page, _ := store.Tail("agt_1", MaxTailLines, 0)
	for i := 0; i < 6; i++ {
		if got := strings.Count(page.Text, fmt.Sprintf(" turn %d line ", i)); got != 50 {
			t.Errorf("turn %d: %d of 50 lines", i, got)
		}
	}
	store.mu.Lock()
	open := len(store.open)
	store.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d writers left open once every turn closed", open)
	}
	// A closed turn takes no more.
	log, _ := store.OpenTurn("agt_1", "late", 1, "normal")
	log.Close()
	if n, err := log.Write([]byte("after close\n")); n != 12 || err != nil {
		t.Fatalf("write after close = %d, %v", n, err)
	}
	if page, _ := store.Tail("agt_1", MaxTailLines, 0); strings.Contains(page.Text, "after close") {
		t.Fatal("a closed turn wrote")
	}
}

func TestSweepRemovesOldOutputAndEnforcesTheTotalWithoutTouchingWhatIsLive(t *testing.T) {
	store, clock := newStore(t)
	for _, agent := range []string{"old", "mid", "new", "live"} {
		write(t, store, agent, "t", strings.Repeat("x", 1000)+"\n")
	}
	age := func(agent string, by time.Duration) {
		path := filepath.Join(store.dir, DirName(agent), FileName)
		when := clock.Add(-by)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	age("old", 20*24*time.Hour)
	age("mid", 2*time.Hour)
	age("new", time.Minute)
	age("live", 3*24*time.Hour)
	live, err := store.OpenTurn("live", "running", 1, "normal") // a turn is capturing now
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	// Retention: the one past 14 days goes.
	result, err := store.Sweep(14*24*time.Hour, 0)
	if err != nil || result.Files != 1 {
		t.Fatalf("age sweep: %+v %v", result, err)
	}
	if _, err := store.Tail("old", 1, 0); err != ErrNoLog {
		t.Fatalf("the old output is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.dir, DirName("old"))); !os.IsNotExist(err) {
		t.Fatalf("its empty directory stayed: %v", err)
	}
	// The cap: room for two files, so the oldest goes first, except what is live or was written moments ago.
	bytes, agents := store.Usage()
	if agents != 3 || bytes < 3000 {
		t.Fatalf("usage %d bytes of %d agents", bytes, agents)
	}
	result, err = store.Sweep(14*24*time.Hour, 2500)
	if err != nil || result.Files != 1 {
		t.Fatalf("cap sweep: %+v %v", result, err)
	}
	if _, err := store.Tail("mid", 1, 0); err != ErrNoLog {
		t.Fatal("the oldest eligible file was not the one removed")
	}
	for _, agent := range []string{"new", "live"} {
		if _, err := store.Tail(agent, 1, 0); err != nil {
			t.Fatalf("%s was removed: %v", agent, err)
		}
	}
	// Still over the cap, but nothing else is eligible: nothing more goes.
	if result, _ := store.Sweep(14*24*time.Hour, 10); result.Files != 0 {
		t.Fatalf("a recent or live file was removed: %+v", result)
	}
	if result, err := New(filepath.Join(t.TempDir(), "none")).Sweep(time.Hour, 1); err != nil || result.Files != 0 {
		t.Fatalf("sweeping a store that does not exist: %+v %v", result, err)
	}
}

func TestOpenTurnNeedsAnAgent(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.OpenTurn("", "t", 1, "normal"); err == nil {
		t.Fatal("an empty agent id was accepted")
	}
}

// The output log is written a line at a time, so a private key that spans lines is dropped as its lines come: the BEGIN line becomes a
// marker, and the lines up to and with the END line are left out. A BEGIN line that is never closed cannot swallow the rest of the log.
func TestAPrivateKeyAcrossLinesIsDroppedFromTheOutputLog(t *testing.T) {
	store, _ := newStore(t)
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU\nAAAAEbm9uZQAAAAAAAAABAAABlwAAAAd\n-----END OPENSSH PRIVATE KEY-----\n"
	write(t, store, "a", "t1", "cat id_ed25519\n"+key+"after the key\n-----BEGIN PRIVATE KEY-----AAAA-----END PRIVATE KEY----- on one line\nlast\n")
	got := readAll(t, store, "a")
	for _, secret := range []string{"b3Blbn", "AAAAEbm9", "AAAA-", "END OPENSSH"} {
		if strings.Contains(got, secret) {
			t.Fatalf("%q reached the log:\n%s", secret, got)
		}
	}
	for _, want := range []string{" cat id_ed25519\n", " [redacted private key]\n", " after the key\n", " [redacted private key] on one line\n", " last\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the log lacks %q:\n%s", want, got)
		}
	}
	var open strings.Builder
	open.WriteString("-----BEGIN RSA PRIVATE KEY-----\n")
	for i := 0; i < privateKeyLines+5; i++ {
		open.WriteString("line of something else\n")
	}
	open.WriteString("shown again\n")
	write(t, store, "b", "t1", open.String())
	if got := readAll(t, store, "b"); !strings.Contains(got, " shown again\n") || strings.Count(got, "line of something else") != 5 {
		t.Fatalf("a key that never ends: %d lines kept, %q", strings.Count(got, "line of something else"), got[max(0, len(got)-120):])
	}
}
