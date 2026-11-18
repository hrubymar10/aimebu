package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// inboxBenchFixture builds a database the size of a production server that has
// been running for a while: ~26k messages, thousands of turns with receipts and
// events, a few hundred open inbox items and several thousand resolved ones.
func inboxBenchFixture(tb testing.TB) *Store {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "aimebu.sqlite")
	if err := os.WriteFile(path, blankTestDB, 0o600); err != nil {
		tb.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			tb.Fatalf("%s: %v", q, err)
		}
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	exec(`INSERT INTO user (id,login,pw_hash,pw_params,created_at) VALUES ('usr_bench','bench','x','y',?)`, now)
	exec(`INSERT INTO project (id,name,folder,created_at) VALUES ('prj_bench','bench','/tmp/bench',?)`, now)
	exec(`INSERT INTO membership (project_id,user_id,role) VALUES ('prj_bench','usr_bench','owner')`)
	exec(`INSERT INTO machine (id,name,key_hash,approved) VALUES ('mch_bench','bench','x',1)`)
	exec(`INSERT INTO checkout (id,project_id,machine_id,canonical_path) VALUES ('cko_bench','prj_bench','mch_bench','/tmp/bench')`)
	exec(`INSERT INTO role (id,key,label,body) VALUES ('rol_bench','bench','Bench','')`)
	exec(`INSERT INTO agent (id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
		VALUES ('agt_bench','bench','prj_bench','rol_bench','claude','code','m','p','mch_bench','cko_bench','x','usr_bench',?)`, now)
	exec(`INSERT INTO session (id,agent_id,generation,start_mode,started_at) VALUES ('ses_bench','agt_bench',0,'fresh_with_history',?)`, now)
	rooms := []string{"rm_bench_home"}
	for i := 1; i <= 29; i++ {
		rooms = append(rooms, fmt.Sprintf("rm_bench_%02d", i))
		exec(`INSERT INTO room (id,project_id,depth,kind,title,created_by,created_at) VALUES (?,?,0,'task',?,'usr_bench',?)`,
			rooms[i], "prj_bench", fmt.Sprintf("room %d", i), now)
	}
	exec(`INSERT INTO room (id,project_id,depth,kind,title,created_by,created_at) VALUES ('rm_bench_home','prj_bench',0,'home','home','usr_bench',?)`, now)
	// 26k messages across the rooms.
	for i := 0; i < 26000; i++ {
		room := rooms[1+i%29]
		kind, author := "agent", "agt_bench"
		if i%5 == 0 {
			kind, author = "user", "usr_bench"
		}
		exec(`INSERT INTO message (id,seq,room_id,author_kind,author_id,body,targets_json,created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("msg_bench_%06d", i), i+1, room, kind, author,
			fmt.Sprintf("message body %d with some words to make it realistic", i), "[]", now)
	}
	// 6000 turns, each with a receipt and a few events; a third have the
	// turn_process_exit event the inbox read looks up.
	for i := 0; i < 6000; i++ {
		room := rooms[1+i%29]
		turn := fmt.Sprintf("trn_bench_%05d", i)
		exec(`INSERT INTO turn (id,agent_id,session_id,generation,room_id,attempt,mode,exec,outcome,started_at,ended_at)
			VALUES (?,'agt_bench','ses_bench',0,?,1,'normal','exited','completed',?,?)`, turn, room, now, now)
		exec(`INSERT INTO receipt (id,turn_id,answer_rev,digest,result_json,created_at) VALUES (?,?,0,'d','{"error":"none"}',?)`,
			fmt.Sprintf("rcp_bench_%05d", i), turn, now)
		exec(`INSERT INTO event (id,seq,kind,project_id,room_id,agent_id,turn_id,payload_json,created_at)
			VALUES (?,?, 'turn_started','prj_bench',?,'agt_bench',?,'{}',?)`,
			fmt.Sprintf("evt_bench_%07d", 2*i+1), 2*i+1, room, turn, now)
		if i%3 == 0 {
			exec(`INSERT INTO event (id,seq,kind,project_id,room_id,agent_id,turn_id,payload_json,created_at)
				VALUES (?,?,'turn_process_exit','prj_bench',?,'agt_bench',?,'{"evidence":"exit","result_text":"ok"}',?)`,
				fmt.Sprintf("evt_bench_%07d", 2*i+2), 2*i+2, room, turn, now)
		} else {
			exec(`INSERT INTO event (id,seq,kind,project_id,room_id,agent_id,turn_id,payload_json,created_at)
				VALUES (?,?,'turn_note','prj_bench',?,'agt_bench',?,'{}',?)`,
				fmt.Sprintf("evt_bench_%07d", 2*i+2), 2*i+2, room, turn, now)
		}
	}
	// 300 open inbox items the way production holds them, plus 3000 resolved
	// ones left behind by history.
	for i := 0; i < 300; i++ {
		room := rooms[1+i%29]
		var kind, requester, options, turnID, messageID string
		switch i % 4 {
		case 0:
			kind, requester, options = "question", "agt_bench", `{"text":"question `+fmt.Sprint(i)+`","options":["A","B"]}`
			messageID = fmt.Sprintf("msg_bench_%06d", i)
		case 1:
			kind, requester, options = "recovery", "system", ""
			turnID = fmt.Sprintf("trn_bench_%05d", i)
		case 2:
			kind, requester, options = "delivery_failed", "agt_bench", ""
			turnID = fmt.Sprintf("trn_bench_%05d", i)
			messageID = fmt.Sprintf("msg_bench_%06d", i)
		case 3:
			kind, requester, options = "approval", "system", `{"text":"approve `+fmt.Sprint(i)+`","options":["Approve","Deny"]}`
		}
		exec(`INSERT INTO inbox_item (id,project_id,room_id,message_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("inx_bench_%04d", i), "prj_bench", room,
			benchNullable(messageID), benchNullable(turnID), benchNullable(benchAgentRef(turnID)), benchRequesterKind(kind), requester, kind, benchNullable(options), now)
		if i%3 == 0 {
			exec(`INSERT INTO inbox_item_seen (item_id,user_id,seen_at) VALUES (?,'usr_bench',?)`, fmt.Sprintf("inx_bench_%04d", i), now)
		}
	}
	for i := 0; i < 3000; i++ {
		exec(`INSERT INTO inbox_item (id,project_id,room_id,message_id,requester_kind,requester_id,kind,state,resolved_by,resolved_at,created_at)
			VALUES (?,?,?,?,?,'agt_bench','question','resolved','usr_bench',?,?)`,
			fmt.Sprintf("inx_old_%05d", i), "prj_bench", rooms[1+i%29],
			fmt.Sprintf("msg_bench_%06d", 3000+i), "agt_bench", now, now)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return s
}

func benchNullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func benchAgentRef(turnID string) string {
	if turnID == "" {
		return ""
	}
	return "agt_bench"
}

func benchRequesterKind(kind string) string {
	if kind == "recovery" || kind == "approval" {
		return "system"
	}
	return "agent"
}

func TestInboxParentModeIndexExists(t *testing.T) {
	s, _ := testStore(t, Options{})
	var name string
	err := s.readDB.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='index' AND name='turn_parent_mode'`).Scan(&name)
	if err != nil {
		t.Fatalf("the turn_parent_mode index is missing from a fresh database: %v", err)
	}
}

func BenchmarkInboxForUser(b *testing.B) {
	s := inboxBenchFixture(b)
	ctx := context.Background()
	items, err := s.InboxForUser(ctx, "usr_bench")
	if err != nil {
		b.Fatal(err)
	}
	if len(items) == 0 {
		b.Fatal("fixture produced no inbox items")
	}
	b.Logf("open items: %d", len(items))
	start := time.Now()
	for b.Loop() {
		items, err := s.InboxForUser(ctx, "usr_bench")
		if err != nil {
			b.Fatal(err)
		}
		if len(items) == 0 {
			b.Fatal("no items")
		}
	}
	b.ReportMetric(float64(time.Since(start).Microseconds())/float64(b.N)/1000, "ms/op")
}

// inboxBenchQuery is the read InboxForUser makes, for the query-plan test.
func inboxBenchQuery() string {
	return `SELECT i.id FROM inbox_item i JOIN room r ON r.id=i.room_id JOIN membership member ON member.project_id=i.project_id
		AND member.user_id='usr_bench'
		WHERE i.state='open' AND i.kind IN ('question','approval','recovery','delivery_failed','usage_info','usage_limit')
		ORDER BY i.created_at,i.id`
}
