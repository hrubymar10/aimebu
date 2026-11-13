package server

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hrubymar10/aimebu/internal/store"
	"modernc.org/sqlite"
)

// queryLog collects the statements one request runs. The counting driver only records a statement whose context carries a log, so the
// background work of a fixture (sweeps, the dispatcher) is never counted.
type queryLog struct {
	mu         sync.Mutex
	statements []string
}

type queryLogKey struct{}

func (l *queryLog) add(query string) {
	l.mu.Lock()
	l.statements = append(l.statements, query)
	l.mu.Unlock()
}

func noteQuery(ctx context.Context, query string) {
	if l, ok := ctx.Value(queryLogKey{}).(*queryLog); ok {
		l.add(query)
	}
}

var countingDriverSeq atomic.Int64

type countingDriver struct{ inner driver.Driver }

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn}, nil
}

type countingConn struct{ driver.Conn }

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	noteQuery(ctx, query)
	return queryer.QueryContext(ctx, query, args)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	noteQuery(ctx, query)
	return execer.ExecContext(ctx, query, args)
}

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	noteQuery(ctx, query)
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *countingConn) Begin() (driver.Tx, error) { return c.Conn.Begin() } //nolint:staticcheck

func (c *countingConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, options)
	}
	return c.Conn.Begin() //nolint:staticcheck
}

func (c *countingConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *countingConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *countingConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *countingConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

// registerCountingDriver registers a copy of the SQLite driver that reports statements to the queryLog in their context.
func registerCountingDriver() string {
	name := fmt.Sprintf("aimebu-counting-%d", countingDriverSeq.Add(1))
	sql.Register(name, countingDriver{inner: &sqlite.Driver{}})
	return name
}

// countingHandler tags each request with a fresh queryLog and keeps the log of the last request whose path matches.
type countingHandler struct {
	mu   sync.Mutex
	last map[string]*queryLog
}

func (h *countingHandler) wrap(next http.Handler) http.Handler {
	h.last = map[string]*queryLog{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := &queryLog{}
		h.mu.Lock()
		h.last[r.URL.Path] = l
		h.mu.Unlock()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), queryLogKey{}, l)))
	})
}

func (h *countingHandler) statementsFor(path string) []string {
	h.mu.Lock()
	l := h.last[path]
	h.mu.Unlock()
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.statements...)
}

// roomRouteStatements are the statements of the request's own work: those the shared preamble of every room route (the session, the room, the
// person's access to it) ran are left out, because they cost the same for every room route and are not what the list's size multiplies.
func roomRouteStatements(statements []string) []string {
	var own []string
	for _, statement := range statements {
		switch {
		case strings.Contains(statement, "FROM auth_session"),
			strings.Contains(statement, "FROM room WHERE id=?"),
			strings.Contains(statement, "SELECT COUNT(*) FROM room r"):
			continue
		}
		own = append(own, statement)
	}
	return own
}

// maxRoomListQueries is how many statements the room's agent and member lists may run, whatever the number of agents and people.
const maxRoomListQueries = 3

// seedRoomAgents adds n agents to the fixture's room and gives the first six a varied history: usage, a running turn, a provider limit, open
// inbox items, a switch, boundary grants. It returns the agents' ids.
func seedRoomAgents(t *testing.T, f *fixture, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		agent, _ := f.createToolAgentNamed(t, "role_worker_default", fmt.Sprintf("seed%c%c", 'a'+i, 'a'+i))
		ids = append(ids, agent.ID)
	}
	if n < 6 {
		return ids
	}
	old := "2026-01-01T00:00:00.000000000Z"
	future := "2999-01-01T00:00:00.000000000Z"
	err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		for i, id := range ids[:6] {
			session := fmt.Sprintf("ses_seed%d", i)
			turn := fmt.Sprintf("trn_seed%d", i)
			exec := "exited"
			if i == 1 {
				exec = "running"
			}
			for _, statement := range []struct {
				query string
				args  []any
			}{
				{`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?,?,0,'fresh_with_history',?)`, []any{session, id, old}},
				{`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,started_at) VALUES (?,?,?,0,?,1,'normal',?,?)`, []any{turn, id, session, f.room.ID, exec, old}},
			} {
				if _, err := tx.Exec(statement.query, statement.args...); err != nil {
					return err
				}
			}
		}
		stmts := []struct {
			query string
			args  []any
		}{
			// 0: a complete report and a later partial one; the card shows the complete one, with the cost of both
			{`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,tokens_in,tokens_out,context_used,context_window,cost_est,source,partial,created_at) VALUES ('use_a','evt_a','trn_seed0',?,?,?,10,20,30,40,0.5,'measured',0,?)`, []any{ids[0], f.room.ID, f.project.ID, old}},
			{`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,tokens_in,tokens_out,cost_est,source,partial,created_at) VALUES ('use_b','evt_b','trn_seed0',?,?,?,1,2,0.25,'estimated',1,?)`, []any{ids[0], f.room.ID, f.project.ID, "2026-01-02T00:00:00.000000000Z"}},
			// 5: only a cost, no complete report
			{`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,cost_est,source,partial,created_at) VALUES ('use_c','evt_c','trn_seed5',?,?,?,1.5,'estimated',1,?)`, []any{ids[5], f.room.ID, f.project.ID, old}},
			// 2: out of quota until far in the future, a held answer and a recovery waiting
			{`UPDATE agent SET tool='codex' WHERE id=?`, []any{ids[2]}},
			{`INSERT INTO provider_limit(agent_id,turn_id,detected_at,reset_at,tool) VALUES (?,'trn_seed2',?,?,'codex')`, []any{ids[2], old, future}},
			{`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,created_at) VALUES ('inb_seed_a',?,?,'trn_seed2',?,'agent',?,'delivery_failed',?)`, []any{f.project.ID, f.room.ID, ids[2], ids[2], old}},
			{`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,created_at) VALUES ('inb_seed_b',?,?,'trn_seed2',?,'agent',?,'recovery',?)`, []any{f.project.ID, f.room.ID, ids[2], ids[2], old}},
			// 3: a failed switch and two boundary grants
			{`INSERT INTO agent_switch(agent_id,room_id,tool,variant,model,provider,state,error,requested_at) VALUES (?,?,'fake','test','other','fake','failed','no capacity',?)`, []any{ids[3], f.room.ID, old}},
			{`INSERT INTO agent_boundary_grant(agent_id,machine_id,canonical_path,approved_by,approved_at) SELECT ?,?,?,id,? FROM user LIMIT 1`, []any{ids[3], f.machineID, "/tmp/zeta", "2026-01-03T00:00:00.000000000Z"}},
			{`INSERT INTO agent_boundary_grant(agent_id,machine_id,canonical_path,approved_by,approved_at) SELECT ?,?,?,id,? FROM user LIMIT 1`, []any{ids[3], f.machineID, "/tmp/alpha", old}},
			// 4 is in a turn, 5 is stopped
			{`UPDATE agent SET observed='in_turn' WHERE id=?`, []any{ids[4]}},
			{`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, []any{ids[5]}},
		}
		for _, statement := range stmts {
			if _, err := tx.Exec(statement.query, statement.args...); err != nil {
				return fmt.Errorf("%s: %w", statement.query, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// legacyRoomAgents is the room's agent list as the handler built it before it was batched: one read of the cards, then each card's usage and health
// on its own. It is the reference the batched list must equal byte for byte.
func legacyRoomAgents(t *testing.T, f *fixture, userID string) []byte {
	t.Helper()
	ctx := t.Context()
	cards, err := f.db.AgentCardsForRoom(ctx, f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if role, roleErr := f.db.UserProjectRole(ctx, userID, f.project.ID); roleErr == nil && role == "viewer" {
		for i := range cards {
			cards[i] = cardWithoutPaths(cards[i])
		}
	}
	withUsage := make([]agentCardWithUsage, 0, len(cards))
	holds := f.srv.agentHolds(ctx, cards)
	keepConfig, _ := f.srv.keepAliveConfig(ctx)
	for _, card := range cards {
		usage, err := f.db.AgentUsage(ctx, card.ID)
		if err != nil {
			t.Fatal(err)
		}
		flags, err := f.srv.agentHealth(ctx, card)
		if err != nil {
			t.Fatal(err)
		}
		withUsage = append(withUsage, agentCardWithUsage{card, usage, flags, holds[card.ID], f.srv.agentWarmState(card), f.srv.agentKeepAliveState(card, usage, keepConfig)})
	}
	recorder := httptest.NewRecorder()
	writeJSON(recorder, 200, withUsage)
	return recorder.Body.Bytes()
}

func TestRoomAgentListEqualsPerAgentReads(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	seedRoomAgents(t, f, 7)
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	response := f.request("GET", "/api/rooms/"+f.room.ID+"/agents", nil, false)
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("agents: %d %v", response.StatusCode, err)
	}
	want := legacyRoomAgents(t, f, owner.ID)
	if !bytes.Equal(got, want) {
		t.Fatalf("the batched agent list differs from the per-agent reads\n got: %s\nwant: %s", got, want)
	}
	// The history seeded above shows up, so the equality is not between two empty answers.
	for _, needle := range []string{`"context_used":30`, `"cost_so_far":0.75`, `"cost_so_far":1.5`, `"code":"stuck"`, `"code":"out_of_quota"`, `"code":"held_answer"`, `"code":"recovery_waiting"`,
		`"code":"machine_offline"`, `"state":"failed"`, `"path":"/tmp/alpha"`} {
		if !strings.Contains(string(got), needle) {
			t.Errorf("the list lacks %s: %s", needle, got)
		}
	}
	if strings.Index(string(got), `"code":"held_answer"`) > strings.Index(string(got), `"code":"recovery_waiting"`) {
		t.Errorf("the health flags are out of order: %s", got)
	}
}

func TestRoomAgentAndMemberListsRunFewStatements(t *testing.T) {
	t.Parallel()
	counter := &countingHandler{}
	f := newFixtureWith(t, store.Options{DriverName: registerCountingDriver()}, counter.wrap)
	f.seed()
	ids := seedRoomAgents(t, f, 7)
	for _, route := range []string{"agents", "members"} {
		path := "/api/rooms/" + f.room.ID + "/" + route
		response := f.request("GET", path, nil, false)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("%s: %d", route, response.StatusCode)
		}
		statements := roomRouteStatements(counter.statementsFor(path))
		if len(ids) < 5 || len(statements) == 0 || len(statements) > maxRoomListQueries {
			t.Errorf("%s with %d agents ran %d statements, want 1 to %d:\n%s", route, len(ids), len(statements), maxRoomListQueries, strings.Join(statements, "\n---\n"))
		}
	}
}

// TestRoomMembersResponseIsUnchanged pins the member list's JSON, byte for byte, including the built-in singleton roles.
func TestRoomMembersResponseIsUnchanged(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	drew, _ := f.createToolAgentNamed(t, "role_worker_default", "drew")
	tessa, _ := f.createToolAgentNamed(t, "role_leader_default", "tessa")
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	response := f.request("GET", "/api/rooms/"+f.room.ID+"/members", nil, false)
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("members: %d %v", response.StatusCode, err)
	}
	want := `{"humans":[{"id":"OWNER","login":"owner","role":"owner"}],"agents":[` +
		`{"id":"DREW","slug":"drew","role_key":"worker","role_label":"Worker","role_emoji":"🛠️","singleton":false,"desired":"active","observed":"idle"},` +
		`{"id":"TESSA","slug":"tessa","role_key":"leader","role_label":"Leader","role_emoji":"👑","singleton":true,"desired":"active","observed":"idle"}],` +
		`"singletons":[{"role_key":"dispatcher","label":"Dispatcher","emoji":"📨","held_by":null},{"role_key":"leader","label":"Leader","emoji":"👑","held_by":{"agent_id":"TESSA","slug":"tessa","in_this_room":true}}]}` + "\n"
	want = strings.NewReplacer("OWNER", owner.ID, "DREW", drew.ID, "TESSA", tessa.ID).Replace(want)
	if string(got) != want {
		t.Fatalf("the member list changed\n got: %s\nwant: %s", got, want)
	}
}
