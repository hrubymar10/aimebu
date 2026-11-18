package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/getsentry/sentry-go"
	"modernc.org/sqlite"
)

type captureTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (c *captureTransport) Flush(time.Duration) bool              { return true }
func (c *captureTransport) FlushWithContext(context.Context) bool { return true }
func (c *captureTransport) Configure(sentry.ClientOptions)        {}
func (c *captureTransport) Close()                                {}
func (c *captureTransport) SendEvent(event *sentry.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}
func (c *captureTransport) sent() []*sentry.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*sentry.Event(nil), c.events...)
}

func startTest(t *testing.T) (*Telemetry, *captureTransport) {
	t.Helper()
	transport := &captureTransport{}
	tel, err := Init(Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	return tel, transport
}

func marshal(t *testing.T, event *sentry.Event) string {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func TestCaptureWarningOnceUsesKeyFingerprint(t *testing.T) {
	_, transport := startTest(t)
	key := "codex-warm-test-key"
	CaptureWarning(context.Background(), key, errors.New("first detail"))
	CaptureWarning(context.Background(), key, errors.New("second detail"))
	events := transport.sent()
	if len(events) != 1 || events[0].Level != sentry.LevelWarning || len(events[0].Fingerprint) != 1 || events[0].Fingerprint[0] != key {
		t.Fatalf("warning events = %+v", events)
	}
	if strings.Contains(marshal(t, events[0]), "second detail") {
		t.Fatal("duplicate warning was captured")
	}
}

func TestFromEnv(t *testing.T) {
	off, err := FromEnv(lookup(nil), "aimebu v2")
	if err != nil || off.Enabled() || off.EnvSet || off.Environment != "production" {
		t.Fatalf("nothing set: %+v %v", off, err)
	}
	on, err := FromEnv(lookup(map[string]string{"AIMEBU_SENTRY_DSN": " https://k@h/1 ", "AIMEBU_SENTRY_ENV": "prod", "AIMEBU_SENTRY_TRACES_SAMPLE_RATE": "0.5"}), "aimebu v2.0.0-3-gabc/x y")
	if err != nil || !on.Enabled() || !on.EnvSet || on.DSN != "https://k@h/1" || on.Environment != "prod" || on.TracesSampleRate != 0.5 || on.Release != "v2.0.0-3-gabc_x_y" {
		t.Fatalf("%+v %v", on, err)
	}
	if off.TracesSampleRate != 1 || DefaultTracesSampleRate != 1 {
		t.Fatalf("the sample rate defaults to all of the requests: %v", off.TracesSampleRate)
	}
	for _, disabled := range []string{"off", "OFF", "", "  "} {
		config, err := FromEnv(lookup(map[string]string{"AIMEBU_SENTRY_DSN": disabled}), "v")
		if err != nil || config.Enabled() || !config.EnvSet {
			t.Errorf("DSN %q must disable and still count as set: %+v %v", disabled, config, err)
		}
	}
	for _, bad := range []string{"2", "-1", "abc"} {
		if _, err := FromEnv(lookup(map[string]string{"AIMEBU_SENTRY_TRACES_SAMPLE_RATE": bad}), "v"); err == nil {
			t.Errorf("rate %q must be rejected", bad)
		}
	}
}

// The precedence: the environment, then Settings; and nothing before the consent popup was answered.
func TestResolvePrecedence(t *testing.T) {
	yes, no := true, false
	saved := DefaultDSN
	defer func() { DefaultDSN = saved }()
	const mine, shipped = "https://a@h.example/1", "https://b@h.example/2"
	env := func(values map[string]string) Config { c, _ := FromEnv(lookup(values), "v"); return c }
	for name, tc := range map[string]struct {
		def    string
		env    map[string]string
		stored Stored
		want   string
		source string
	}{
		"nothing set, no default":              {"", nil, Stored{Prompted: true}, "", "settings"},
		"not answered yet sends nothing":       {shipped, nil, Stored{}, "", "pending"},
		"answered, default on":                 {shipped, nil, Stored{Prompted: true}, shipped, "settings"},
		"answered, chosen off":                 {shipped, nil, Stored{Prompted: true, Enabled: &no}, "", "settings"},
		"answered, own DSN":                    {shipped, nil, Stored{Prompted: true, DSN: mine}, mine, "settings"},
		"answered, no default, toggle on only": {"", nil, Stored{Prompted: true, Enabled: &yes}, "", "settings"},
		"answered, no default, own DSN on":     {"", nil, Stored{Prompted: true, Enabled: &yes, DSN: mine}, mine, "settings"},
		"env DSN wins over everything":         {shipped, map[string]string{"AIMEBU_SENTRY_DSN": mine}, Stored{Prompted: true, Enabled: &no}, mine, "env"},
		"env DSN needs no popup":               {shipped, map[string]string{"AIMEBU_SENTRY_DSN": mine}, Stored{}, mine, "env"},
		"env off wins over a default":          {shipped, map[string]string{"AIMEBU_SENTRY_DSN": "off"}, Stored{Prompted: true}, "", "env"},
		"env empty wins over a default":        {shipped, map[string]string{"AIMEBU_SENTRY_DSN": ""}, Stored{Prompted: true, DSN: mine}, "", "env"},
	} {
		DefaultDSN = tc.def
		got := env(tc.env).Resolve(tc.stored)
		if got.DSN != tc.want || got.Source != tc.source {
			t.Errorf("%s: DSN %q source %q, want %q %q", name, got.DSN, got.Source, tc.want, tc.source)
		}
	}
}

// Without a shipped default (this file's value; a build adds one in default_dsn.go) the build is off until a DSN is chosen, and the popup's
// toggle starts off.
func TestWithoutDefaultDSNTheBuildIsOff(t *testing.T) {
	saved := DefaultDSN
	DefaultDSN = ""
	defer func() { DefaultDSN = saved }()
	config := Config{}.Resolve(Stored{Prompted: true})
	if config.Enabled() || (Stored{}).EnabledOrDefault() || (Stored{}).DSNOrDefault() != "" {
		t.Fatalf("no default DSN must mean off: %+v", config)
	}
}

func TestValidDSN(t *testing.T) {
	if err := ValidDSN("https://f6d48ca3b920b6b2cb3cb0d719305c81@o4512387147563008.ingest.de.sentry.io/4512387159425104"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "x", "https://host/1", "ftp://k@h/1", "https://k@h/", "http://k@sentry.example.com/1", "http://k@10.0.0.5/1", "http://k@localhost.example.com/1"} {
		if ValidDSN(bad) == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	// http is for a Sentry on this machine only.
	for _, good := range []string{"http://k@127.0.0.1:9000/1", "http://k@localhost:9000/1", "http://k@[::1]:9000/1", "https://k@sentry.example.com/1"} {
		if err := ValidDSN(good); err != nil {
			t.Errorf("%q must be accepted: %v", good, err)
		}
	}
}

func TestScrubURL(t *testing.T) {
	s := Scrubber{}
	for in, want := range map[string]string{
		"https://user:pw@host.example:8080/api/rooms/42?token=abc&q=secret#frag": "/api/rooms/42",
		"/api/x?cookie=1": "/api/x",
		"/plain":          "/plain",
		"":                "",
		"::not a url":     "",
	} {
		if got := s.URL(in); got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScrubText(t *testing.T) {
	s := Scrubber{Home: "/Users/alice"}
	got := s.Text(`open /Users/alice/.aimebu/x: denied; Authorization: Bearer abc.def-1 ; token=hunter2 password: "p w" api_key=zzz key aak_0123456789abcdef machine amk_0123456789abcdef session as_0123456789abcdef anthropic sk-ant-api03-ABCDEFGH12345678 openai sk-proj-ABCDEFGH12345678`)
	for _, leak := range []string{"/Users/alice", "abc.def-1", "hunter2", "p w", "zzz", "aak_0123", "amk_0123", "as_0123", "sk-ant-api03", "sk-proj-ABCD"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked in %q", leak, got)
		}
	}
	if !strings.Contains(got, "~/.aimebu/x") {
		t.Errorf("home must show as ~: %q", got)
	}
	if long := s.Text(strings.Repeat("é", 1000)); len(long) > maxText+4 {
		t.Errorf("text must be bounded, got %d bytes", len(long))
	}
}

func TestScrubEvent(t *testing.T) {
	s := Scrubber{Home: "/Users/alice"}
	event := &sentry.Event{
		Message:    "boom for /Users/alice/x",
		ServerName: "alices-macbook",
		User:       sentry.User{ID: "1", Email: "a@b.c", IPAddress: "10.0.0.1"},
		Tags:       map[string]string{"server_name": "alices-macbook", "http.route": "GET /x"},
		Modules:    map[string]string{"x": "1"},
		Request: &sentry.Request{Method: "POST", URL: "https://h.example/api/rooms/1/messages?token=zzz", Data: `{"body":"secret prompt"}`, Cookies: "sid=1", QueryString: "token=zzz",
			Headers: map[string]string{"Authorization": "Bearer abc", "Cookie": "sid=1"}, Env: map[string]string{"HOME": "/Users/alice"}},
		Contexts: map[string]sentry.Context{"trace": {"trace_id": "t", "data": map[string]interface{}{"url.full": "https://h/x?token=secret", "http.route": "GET /x", "deviceMemory": "16 GB"}}, "os": {"name": "darwin"}, "app": {"app_start_time": "x"}, "device": {"model": "mac"}, "custom": {"body": "secret"}},
		Exception: []sentry.Exception{{Value: "read /Users/alice/.ssh/id: token=abc", Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{
			{AbsPath: "/Users/alice/xcode/aimebu/a.go", Filename: "/Users/alice/xcode/aimebu/a.go", Vars: map[string]interface{}{"prompt": "secret"}, ContextLine: "secret := 1", PreContext: []string{"x"}}}}}},
		Breadcrumbs: []*sentry.Breadcrumb{{Category: "console", Message: "log of a message body"}, {Category: "http", Message: "GET /x", Data: map[string]interface{}{"url": "/x?token=1", "body": "secret"}}},
		Transaction: "GET /api/x", Type: "transaction",
		Spans: []*sentry.Span{{Description: "select", Tags: map[string]string{"a": "b"}, Data: map[string]interface{}{"db.statement": "SELECT 1 WHERE a = ?", "db.rows": int64(1), "db.params": []any{"secret"}, "url.path": "/x?token=1", "http.request.body": "secret"}}},
	}
	out := marshal(t, s.Event(event))
	for _, leak := range []string{"alices-macbook", "a@b.c", "10.0.0.1", "secret", "sid=1", "Bearer abc", "token=zzz", "token=abc", "/Users/alice", "h.example", "hunter", "mac\"", "url.full", "deviceMemory", "custom", "console", "log of a message body", "app_start_time"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q survived the scrub: %s", leak, out)
		}
	}
	for _, kept := range []string{"/api/rooms/1/messages", "~/xcode/aimebu/a.go", "darwin", "trace_id", "SELECT 1 WHERE a = ?", "db.rows", "GET /api/x"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q must survive: %s", kept, out)
		}
	}
	if user, _ := json.Marshal(s.Event(event).User); string(user) != `{"id":"1","ip_address":"0.0.0.0"}` {
		t.Errorf("of a user only the id survives: %s", user)
	}
	if s.Event(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}

func TestMiddlewareReportsPanicAndRepanics(t *testing.T) {
	_, transport := startTest(t)
	handler := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom /Users/x token=abc") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic must be raised again so net/http answers it as before")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/boom?secret=1", nil))
	}()
	var found bool
	for _, event := range transport.sent() {
		if len(event.Exception) > 0 || strings.Contains(event.Message, "kaboom") {
			found = true
			// The panic text was "kaboom /Users/x token=abc" and the request "?secret=1": look for those, not for a bare "abc", which random hex ids
			// (trace, span, event) contain by chance about one run in a hundred.
			if out := marshal(t, event); strings.Contains(out, "token=abc") || strings.Contains(out, "/Users/x") || strings.Contains(out, "secret=1") {
				t.Errorf("leak: %s", out)
			}
		}
	}
	if !found {
		t.Fatalf("no error event for the panic: %d events", len(transport.sent()))
	}
}

func TestMiddlewareAbortHandlerIsNotReported(t *testing.T) {
	_, transport := startTest(t)
	handler := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	}()
	for _, event := range transport.sent() {
		if len(event.Exception) > 0 {
			t.Fatal("http.ErrAbortHandler is a deliberate abort, not an error")
		}
	}
}

func TestMiddleware5xxAndTransaction(t *testing.T) {
	_, transport := startTest(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/things/{id}", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "db said secret", 500) })
	mux.HandleFunc("GET /api/ok", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("fine")) })
	handler := Middleware(mux)
	for _, path := range []string{"/api/things/7?token=zzz", "/api/ok"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	}
	Current().Flush(time.Second)
	var message, transactions int
	for _, event := range transport.sent() {
		out := marshal(t, event)
		if strings.Contains(out, "zzz") || strings.Contains(out, "db said secret") {
			t.Errorf("leak: %s", out)
		}
		if event.Type == "transaction" {
			transactions++
			if event.Transaction != "GET /api/things/{id}" && event.Transaction != "GET /api/ok" {
				t.Errorf("transaction named by the route pattern, got %q", event.Transaction)
			}
		} else if strings.HasPrefix(event.Message, "HTTP 500 GET /api/things/{id}") {
			message++
		}
	}
	if message != 1 || transactions != 2 {
		t.Fatalf("want one 5xx message and two transactions, got %d and %d", message, transactions)
	}
}

func TestHandlerErrorReplacesGeneric5xxReport(t *testing.T) {
	_, transport := startTest(t)
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(interface{ CaptureHTTPError(error) }).CaptureHTTPError(errors.New("database failed"))
		http.Error(w, "internal server error", 500)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/fail", nil))
	Current().Flush(time.Second)
	var errorsSeen, generic int
	for _, event := range transport.sent() {
		if len(event.Exception) > 0 && strings.Contains(event.Exception[0].Value, "database failed") {
			errorsSeen++
		}
		if strings.HasPrefix(event.Message, "HTTP 500") {
			generic++
		}
	}
	if errorsSeen != 1 || generic != 0 {
		t.Fatalf("underlying errors=%d generic reports=%d", errorsSeen, generic)
	}
}

func TestMiddlewareKeepsFlusherAndHijacker(t *testing.T) {
	startTest(t)
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush through the wrapper: %v", err)
		}
		if _, ok := w.(http.Flusher); !ok {
			t.Error("the wrapper must stay an http.Flusher")
		}
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("the wrapper must stay an http.Hijacker")
		}
	}))
	req := httptest.NewRequest("GET", "/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestSQLSpansRecordQueryAndRowsNeverValues(t *testing.T) {
	_, transport := startTest(t)
	name := RegisterSQLDriver(&sqlite.Driver{})
	db, err := sql.Open(name, "file:"+t.TempDir()+"/t.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Outside a traced request nothing is spanned.
	if _, err := db.Exec(`CREATE TABLE t(a TEXT)`); err != nil {
		t.Fatal(err)
	}
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, value := range []string{"alpha-secret", "beta-secret"} {
			if _, err := db.ExecContext(r.Context(), `INSERT INTO t(a) VALUES (?)`, value); err != nil {
				t.Error(err)
			}
		}
		rows, err := db.QueryContext(r.Context(), `SELECT a FROM t WHERE a <> ?`, "gamma-secret")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		_ = rows.Close()
		stmt, err := db.PrepareContext(r.Context(), `SELECT count(*) FROM t WHERE a = ?`)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := stmt.QueryRowContext(r.Context(), "delta-secret").Scan(&n); err != nil {
			t.Fatal(err)
		}
		_ = stmt.Close()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/db", nil))
	var spans []*sentry.Span
	for _, event := range transport.sent() {
		if event.Type == "transaction" {
			spans = append(spans, event.Spans...)
			if out := marshal(t, event); strings.Contains(out, "-secret") {
				t.Fatalf("a parameter value reached the transaction: %s", out)
			}
		}
	}
	var selectRows int64 = -1
	for _, span := range spans {
		if span.Op != "db.query" {
			t.Errorf("db span op = %q", span.Op)
		}
		if strings.HasPrefix(span.Data["db.statement"].(string), "SELECT a FROM") {
			selectRows, _ = span.Data["db.rows"].(int64)
		}
	}
	if len(spans) < 4 || selectRows != 2 {
		t.Fatalf("want >=4 db spans and 2 rows on the select, got %d spans, rows=%d", len(spans), selectRows)
	}
	// The description of a db span is its statement (placeholders, no values), not the generic operation name.
	descriptions := map[string]bool{}
	for _, span := range spans {
		descriptions[span.Description] = true
	}
	for _, want := range []string{"INSERT INTO t(a) VALUES (?)", "SELECT a FROM t WHERE a <> ?", "SELECT count(*) FROM t WHERE a = ?"} {
		if !descriptions[want] {
			t.Errorf("no db span described as %q; descriptions: %v", want, descriptions)
		}
	}
	if descriptions["db.query"] {
		t.Errorf("a db span still carries the generic description: %v", descriptions)
	}
}

func TestNormalizeSQL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"SELECT a\n\t FROM t\n WHERE b = ?", "SELECT a FROM t WHERE b = ?"},
		{"  SELECT 1  ", "SELECT ?"},
		{"SELECT a FROM t WHERE b = 'it''s secret' AND c = 42 AND d = 3.5 AND e = ?", "SELECT a FROM t WHERE b = ? AND c = ? AND d = ? AND e = ?"},
		{"SELECT x2, t1.y FROM t1 WHERE k = ?", "SELECT x2, t1.y FROM t1 WHERE k = ?"},
	} {
		if got := NormalizeSQL(tc.in); got != tc.want {
			t.Errorf("NormalizeSQL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := NormalizeSQL("SELECT " + strings.Repeat("é", 2000))
	if len(long) > maxSQLDescription+len("…") || !strings.HasSuffix(long, "…") || !utf8.ValidString(long) {
		t.Errorf("a long statement is cut to %d bytes, valid=%v", len(long), utf8.ValidString(long))
	}
}

// A request that asks the database two hundred times gets MaxDBSpans spans, and says how many it did not span.
func TestSQLSpansPerRequestAreCapped(t *testing.T) {
	_, transport := startTest(t)
	name := RegisterSQLDriver(&sqlite.Driver{})
	db, err := sql.Open(name, "file:"+t.TempDir()+"/t.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	const calls = 200
	Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < calls; i++ {
			rows, err := db.QueryContext(r.Context(), `SELECT ?`, i)
			if err != nil {
				t.Error(err)
				return
			}
			_ = rows.Close()
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/many", nil))
	// A quiet request carries no counters.
	Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows, _ := db.QueryContext(r.Context(), `SELECT 1`)
		_ = rows.Close()
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/few", nil))
	var transactions []*sentry.Event
	for _, event := range transport.sent() {
		if event.Type == "transaction" {
			transactions = append(transactions, event)
		}
	}
	if len(transactions) != 2 {
		t.Fatalf("want 2 transactions, got %d", len(transactions))
	}
	many, few := transactions[0], transactions[1]
	if len(many.Spans) != MaxDBSpans {
		t.Fatalf("%d queries made %d spans, want exactly the cap %d", calls, len(many.Spans), MaxDBSpans)
	}
	data, _ := many.Contexts["trace"]["data"].(map[string]interface{})
	if data["db.calls"] != int64(calls) || data["db.calls_dropped"] != int64(calls-MaxDBSpans) {
		t.Fatalf("the transaction counts what it left out: %v", data)
	}
	if len(few.Spans) != 1 {
		t.Fatalf("a quiet request spans its one query: %d", len(few.Spans))
	}
	if _, has := few.Contexts["trace"]["data"].(map[string]interface{})["db.calls_dropped"]; has {
		t.Fatalf("a quiet request drops nothing: %v", few.Contexts["trace"]["data"])
	}
}

// A request names its person by one id, on transactions, on 5xx reports and on panics; the user carries that id and nothing else.
func TestRequestsCarryOnlyThePersonsReportID(t *testing.T) {
	_, transport := startTest(t)
	mux := http.NewServeMux()
	named := func(id string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			SetUser(r.Context(), id) // what the auth hook does once the session is known
			if r.URL.Query().Get("fail") != "" {
				http.Error(w, "boom", 500)
			}
		}
	}
	mux.HandleFunc("GET /api/a", named("0123456789abcdef"))
	mux.HandleFunc("GET /api/b", named("carol"))
	mux.HandleFunc("GET /api/anon", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /api/panic", func(w http.ResponseWriter, r *http.Request) {
		SetUser(r.Context(), "0123456789abcdef")
		panic("kaboom")
	})
	handler := Middleware(mux)
	for _, path := range []string{"/api/a", "/api/a?fail=1", "/api/b", "/api/anon"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/panic", nil))
	}()
	Current().Flush(time.Second)
	seen := map[string][]string{}
	for _, event := range transport.sent() {
		user, _ := json.Marshal(event.User)
		kind := event.Type
		if kind == "" {
			kind = "event"
		}
		seen[kind] = append(seen[kind], event.Transaction+" "+string(user))
		if strings.Contains(marshal(t, event), "aimebu.user") {
			t.Errorf("the user attribute must not reach the span data: %s", marshal(t, event))
		}
	}
	want := map[string]bool{"GET /api/a {\"id\":\"0123456789abcdef\",\"ip_address\":\"0.0.0.0\"}": true, "GET /api/b {\"id\":\"carol\",\"ip_address\":\"0.0.0.0\"}": true, "GET /api/anon {\"ip_address\":\"0.0.0.0\"}": true}
	for _, got := range seen["transaction"] {
		delete(want, got)
	}
	if len(want) != 0 {
		t.Fatalf("transactions %v, missing %v", seen["transaction"], want)
	}
	var reports int
	for _, event := range transport.sent() {
		if event.Type != "transaction" && (strings.HasPrefix(event.Message, "HTTP 500") || event.Message == "kaboom") {
			reports++
			if user, _ := json.Marshal(event.User); string(user) != `{"id":"0123456789abcdef","ip_address":"0.0.0.0"}` {
				t.Errorf("a 5xx or panic report names its person by the id only: %s", user)
			}
		}
	}
	if reports != 2 {
		t.Fatalf("want the 5xx and the panic, got %d", reports)
	}
}

func TestBrowserConfig(t *testing.T) {
	startTest(t)
	config := Current().BrowserConfig()
	if config["dsn"] != "https://public@example.invalid/1" || config["environment"] != "test" || config["release"] != "v1" {
		t.Fatalf("%v", config)
	}
}

func TestInitRefusesTheNetworkInATest(t *testing.T) {
	if tel, err := Init(Config{DSN: "https://public@example.invalid/1"}, nil); err == nil || tel != nil {
		t.Fatal("a test binary must never get the real transport")
	}
}

func TestInitReplacesAndStopsTheRunningReporter(t *testing.T) {
	first, _ := startTest(t)
	second, err := Init(Config{DSN: "https://public@example.invalid/2", Release: "v2"}, &captureTransport{})
	if err != nil || Current() != second || second == first {
		t.Fatalf("a new config must replace the running reporter: %v %v", second, err)
	}
	if _, err := Init(Config{}, nil); err != nil || Current() != nil {
		t.Fatal("a config without a DSN must stop reporting")
	}
}

func TestInitKeepsTheRunningReporterForTheSameConfig(t *testing.T) {
	cfg := Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}
	first, err := Init(cfg, &captureTransport{})
	if err != nil || first == nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Without a transport the real one would be opened, which a test never does; the same-config check comes first, so this is the path Settings takes.
	same, err := Init(cfg, nil)
	if err != nil || same != first || Current() != first {
		t.Fatalf("an unchanged config must keep the running reporter: %v %v", same == first, err)
	}
	if other, _ := Init(Config{DSN: "http://k@example.com/1"}, &captureTransport{}); other != nil {
		t.Fatal("a plain-http DSN to another host must be refused")
	}
}

// A page load fetches ~240 files of the app through the catch-all route; each was a transaction named "GET /" and together they were 60% of a server's
// transactions. The API and the pages the app is opened by are traced, the files are not, and a failure in a file is still reported.
func TestStaticFilesOfTheAppAreNotTraced(t *testing.T) {
	_, transport := startTest(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ok", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("fine")) })
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/js/broken.js" {
			http.Error(w, "nope", 500)
			return
		}
		_, _ = w.Write([]byte("file"))
	})
	handler := Middleware(mux)
	files := []string{"/js/app.js?v=abc123", "/js/pages/chat/store.js", "/style.css", "/theme-v1.css", "/icons/close.svg", "/vendor/sentry/bundle.tracing.min.js", "/favicon.svg", "/manifest.webmanifest", "/js/broken.js"}
	for _, path := range files {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	traced := []string{"/", "/index.html", "/setup", "/api/ok"}
	for _, path := range traced {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	Current().Flush(time.Second)
	var transactions []string
	var messages int
	for _, event := range transport.sent() {
		switch {
		case event.Type == "transaction":
			transactions = append(transactions, event.Transaction)
		case strings.HasPrefix(event.Message, "HTTP 500"):
			messages++
		}
	}
	if len(transactions) != len(traced) {
		t.Fatalf("want a transaction for each of the %d pages and API calls and none for the %d files, got %d: %v", len(traced), len(files), len(transactions), transactions)
	}
	if messages != 1 {
		t.Fatalf("a 500 from a file is still reported: %d messages", messages)
	}
}

func TestTracedPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/rooms/x/agents": true, "/api/events": true, "/": true, "/index.html": true, "/setup": true,
		"/js/app.js": false, "/css/a.css": false, "/icons/a.svg": false, "/vendor/x.js": false, "/favicon.svg": false, "/api": false, "/apix": false, "/robots.txt": false,
	} {
		if got := tracedPath(path); got != want {
			t.Errorf("tracedPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestMiddlewareNamesRequestsByRouteNeverByPath(t *testing.T) {
	_, transport := startTest(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/switcher/{tool}/profiles/{name}", func(w http.ResponseWriter, r *http.Request) {})
	handler := Middleware(mux)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/switcher/claude/profiles/my-private-profile?x=1", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/no/such/secret-thing", nil))
	Current().Flush(time.Second)
	var names []string
	for _, event := range transport.sent() {
		if event.Type == "transaction" {
			names = append(names, event.Transaction)
		}
		if out := marshal(t, event); strings.Contains(out, "my-private-profile") || strings.Contains(out, "secret-thing") {
			t.Errorf("a path leaked: %s", out)
		}
	}
	if len(names) != 2 || names[0] != "GET /api/switcher/{tool}/profiles/{name}" || names[1] != "GET (unrouted)" {
		t.Fatalf("transactions are named by route pattern: %v", names)
	}
}

// A transaction keeps its own trace identity: the root's trace id, span id, operation, route and status are what was recorded, and the SQL
// child spans carry the same trace id and name the root as their parent. (The hub's scope used to write its own propagation context over them.)
func TestTransactionKeepsItsTraceIdentity(t *testing.T) {
	_, transport := startTest(t)
	name := RegisterSQLDriver(&sqlite.Driver{})
	db, err := sql.Open(name, "file:"+t.TempDir()+"/t.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/things/{id}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_ = db.QueryRowContext(r.Context(), `SELECT 1`).Scan(&n)
		w.WriteHeader(http.StatusTeapot)
	})
	handler := Middleware(mux)
	for i := 0; i < 2; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/things/7", nil))
	}
	Current().Flush(time.Second)
	traces := map[string]bool{}
	var count int
	for _, event := range transport.sent() {
		if event.Type != "transaction" {
			continue
		}
		count++
		trace := fmt.Sprint(event.Contexts["trace"]["trace_id"])
		if event.Contexts["trace"]["trace_id"] == nil || event.Contexts["trace"]["op"] != "http.server" || event.Contexts["trace"]["span_id"] == nil {
			t.Fatalf("the transaction's trace context was overwritten: %v", event.Contexts["trace"])
		}
		if event.Contexts["trace"]["status"] == nil {
			t.Fatalf("the transaction lost its status: %v", event.Contexts["trace"])
		}
		traces[trace] = true
		if len(event.Spans) == 0 {
			t.Fatal("no db span")
		}
		for _, span := range event.Spans {
			if span.TraceID.String() != trace || span.ParentSpanID.String() != fmt.Sprint(event.Contexts["trace"]["span_id"]) {
				t.Fatalf("child span is orphaned: span trace %s parent %s, root %v %v", span.TraceID, span.ParentSpanID, trace, event.Contexts["trace"]["span_id"])
			}
		}
	}
	if count != 2 || len(traces) != 2 {
		t.Fatalf("two requests are two traces, got %d transactions and %d trace ids", count, len(traces))
	}
}

// Errors, panics and 5xx answers carry the trace of the request they happened in, so they can be found from its transaction.
func TestErrorsAreLinkedToTheirRequestsTrace(t *testing.T) {
	_, transport := startTest(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	mux.HandleFunc("GET /api/fail", func(w http.ResponseWriter, r *http.Request) {
		Current().CaptureError(r.Context(), errors.New("handled error"))
		http.Error(w, "x", 500)
	})
	handler := Middleware(mux)
	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/boom", nil))
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/fail", nil))
	Current().Flush(time.Second)
	transactionTrace := map[string]string{} // transaction name -> trace id
	for _, event := range transport.sent() {
		if event.Type == "transaction" {
			transactionTrace[event.Transaction] = fmt.Sprint(event.Contexts["trace"]["trace_id"])
		}
	}
	linked := map[string]string{}
	for _, event := range transport.sent() {
		if event.Type == "transaction" {
			continue
		}
		trace := fmt.Sprint(event.Contexts["trace"]["trace_id"])
		switch {
		case strings.Contains(event.Message, "HTTP 500"):
			linked["5xx"] = trace
		case event.Message == "kaboom": // a string panic is reported as a message
			linked["panic"] = trace
		case len(event.Exception) > 0 && strings.Contains(event.Exception[0].Value, "handled error"):
			linked["error"] = trace
		}
	}
	if len(linked) != 3 {
		t.Fatalf("want a panic, an error and a 5xx event, got %v", linked)
	}
	for kind, want := range map[string]string{"panic": transactionTrace["GET /api/boom"], "error": transactionTrace["GET /api/fail"], "5xx": transactionTrace["GET /api/fail"]} {
		if want == "" || linked[kind] != want {
			t.Errorf("the %s event is not in its request's trace: event %q, transaction %q", kind, linked[kind], want)
		}
	}
}

// No stack frame names the machine, wherever the program was built or run: not this process's HOME (a test's is a temporary directory, a
// service's is not the developer's), not the checkout the binary was built from, not the Go toolchain's directory, and not anyone's home by
// shape. A string panic is reported as a message with threads[].stacktrace, an error panic and a captured error as exception[].stacktrace.
func TestStackFramesNameNoMachinePath(t *testing.T) {
	_, transport := startTest(t)
	_, thisFile, _, _ := runtime.Caller(0)
	moduleRoot := strings.TrimSuffix(filepath.ToSlash(thisFile), "internal/telemetry/telemetry_test.go")
	home, _ := os.UserHomeDir()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /string", func(http.ResponseWriter, *http.Request) { panic("a string panic") })
	mux.HandleFunc("GET /error", func(http.ResponseWriter, *http.Request) { panic(errors.New("an error panic")) })
	mux.HandleFunc("GET /captured", func(w http.ResponseWriter, r *http.Request) {
		Current().CaptureError(r.Context(), errors.New("captured"))
	})
	handler := Middleware(mux)
	for _, path := range []string{"/string", "/error", "/captured"} {
		func() {
			defer func() { _ = recover() }()
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
		}()
	}
	Current().Flush(time.Second)
	frames := 0
	for _, event := range transport.sent() {
		if event.Type == "transaction" {
			continue
		}
		out := marshal(t, event)
		for _, leak := range []string{home, moduleRoot, runtime.GOROOT(), "/Users/", "/home/"} {
			if len(leak) > 1 && strings.Contains(out, leak) {
				t.Errorf("%q must not be sent: %s", leak, out)
			}
		}
		var stacks [][]sentry.Frame
		for _, exception := range event.Exception {
			if exception.Stacktrace != nil {
				stacks = append(stacks, exception.Stacktrace.Frames)
			}
		}
		for _, thread := range event.Threads {
			if thread.Stacktrace != nil {
				stacks = append(stacks, thread.Stacktrace.Frames)
			}
		}
		for _, stack := range stacks {
			for _, frame := range stack {
				if strings.Contains(frame.AbsPath, "internal/telemetry/") {
					frames++
					if strings.HasPrefix(frame.AbsPath, "/") {
						t.Errorf("a frame of this module is not relative: %q", frame.AbsPath)
					}
				}
			}
		}
	}
	if frames == 0 {
		t.Fatal("no frame of this module was reported, so nothing was checked")
	}
}

func TestScrubberPathsAreMachineIndependent(t *testing.T) {
	s := Scrubber{Home: "/Users/alice", ModuleRoot: "/build/checkout/aimebu/", GoRoot: "/opt/go"}
	for in, want := range map[string]string{
		"/build/checkout/aimebu/internal/server/server.go": "internal/server/server.go",
		"/opt/go/src/net/http/server.go":                   "$GOROOT/src/net/http/server.go",
		"/Users/alice/go/pkg/mod/x@v1/a.go":                "~/go/pkg/mod/x@v1/a.go",
		"/Users/someone-else/xcode/aimebu/internal/a.go":   "~/xcode/aimebu/internal/a.go",
		"/home/runner/work/aimebu/aimebu/internal/a.go":    "~/work/aimebu/aimebu/internal/a.go",
		`C:\Users\Bob\src\aimebu\a.go`:                     `~\src\aimebu\a.go`,
		"open /Users/other/.aimebu2/x: permission denied":  "open ~/.aimebu2/x: permission denied",
	} {
		if got := s.path(in); got != want {
			t.Errorf("path(%q) = %q, want %q", in, got, want)
		}
	}
}

// Sentry derives a location from the connecting IP when the payload has no user address, so every event and transaction names one.
func TestEventsCarryNonRoutableAddress(t *testing.T) {
	s := Scrubber{}
	for _, event := range []*sentry.Event{{Message: "boom"}, {Type: "transaction", Transaction: "GET /x"}, {Message: "x", User: sentry.User{IPAddress: "203.0.113.9"}}} {
		out := marshal(t, s.Event(event))
		if !strings.Contains(out, `"ip_address":"0.0.0.0"`) || strings.Contains(out, "203.0.113.9") {
			t.Errorf("the user address must be the non-routable one: %s", out)
		}
	}
}
