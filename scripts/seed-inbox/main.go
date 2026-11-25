package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/store"
	"modernc.org/sqlite"
)

// Seeds API-backed inbox items for the isolated browser smoke server.

func main() {
	if len(os.Args) != 5 && len(os.Args) != 6 && len(os.Args) != 7 {
		fmt.Fprintln(os.Stderr, "usage: seed-inbox DB PROJECT ROOM MESSAGE [recovery|budget AGENT|agentchat AGENT]")
		os.Exit(2)
	}
	if (len(os.Args) == 6 && os.Args[5] != "recovery" && os.Args[5] != "legacy") || (len(os.Args) == 7 && os.Args[5] != "budget" && os.Args[5] != "agentchat") {
		fmt.Fprintln(os.Stderr, "unknown seed mode")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// Store.Open configures SQLite's five-second busy timeout. The smoke
	// server can still own the writer past one attempt, so retry the whole
	// transaction if it encounters BUSY or LOCKED.
	db, err := store.Open(ctx, os.Args[1], store.Options{})
	if err != nil {
		panic(err)
	}
	defer db.Close()
	agentChat := len(os.Args) == 7 && os.Args[5] == "agentchat"
	var machine string
	if agentChat {
		if machine, _, err = db.AgentPlacement(ctx, os.Args[6]); err != nil {
			panic(err)
		}
	}
	seed := func() error {
		return db.Tx(ctx, func(tx *store.Tx) error {
			if agentChat {
				// A second project with one agent, and a private chat between that agent and the given one (which belongs to the smoke project).
				// The one message is the system's, so no agent is woken: the browser smoke is about what people see and may do.
				other, err := tx.CreateProject(store.Project{Name: "Other project", Folder: "/tmp/aimebu-smoke-other"})
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_smoke_chat',?,?,'/tmp/aimebu-smoke-other')`, other.ID, machine); err != nil {
					return err
				}
				peer, err := tx.CreateAgent(store.Agent{Slug: "peer", ProjectID: other.ID, RoleID: "role_worker_default", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: machine, CheckoutID: "chk_smoke_chat", KeyHash: "placeholder", CreatedBy: "seed"})
				if err != nil {
					return err
				}
				chat, _, err := tx.OpenAgentDM(os.Args[6], peer.ID, os.Args[6])
				if err != nil {
					return err
				}
				_, err = tx.InsertMessage(store.Message{RoomID: chat.ID, AuthorKind: "system", AuthorID: "system", Body: "The two agents agreed on the API shape."})
				return err
			}
			if len(os.Args) == 7 {
				updated, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=?`, os.Args[6])
				if err != nil {
					return err
				}
				count, err := updated.RowsAffected()
				if err != nil || count != 1 {
					return fmt.Errorf("budget agent not found: %v", err)
				}
				if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
				VALUES (?,?,?,?,'agent',?,'recovery',?,datetime('now'))`, "inb_smoke_budget", os.Args[2], os.Args[3], os.Args[6], os.Args[6], `{"text":"Agent paused after too many turns"}`); err != nil {
					return err
				}
				_, err = tx.AppendEvent(store.Event{Kind: "inbox_item_created", ProjectID: os.Args[2], RoomID: os.Args[3], AgentID: os.Args[6], PayloadJSON: `{"inbox_item_id":"inb_smoke_budget"}`})
				return err
			}
			if len(os.Args) == 6 && os.Args[5] == "legacy" {
				// An agent that exists only in imported v1 history: its session, the role v1 gave it, and a message of its own in the room.
				for _, statement := range []string{
					`INSERT OR IGNORE INTO legacy_agent_session(agent_id,harness,model,project,cwd,harness_session_id,registered_at,last_seen) VALUES ('veteran@old','claude-code','haiku','old','/definitely/not/in/the/project','0f7c6a1e-1111-4222-8333-444455556666','2026-10-01T10:00:00Z','2026-10-02T10:00:00Z')`,
					`INSERT OR IGNORE INTO legacy_agent_role(agent_id,v1_room_id,role_key) VALUES ('veteran@old','old-room','reviewer')`,
				} {
					if _, err := tx.Exec(statement); err != nil {
						return err
					}
				}
				_, err := tx.InsertMessage(store.Message{RoomID: os.Args[3], AuthorKind: "agent", AuthorID: "veteran@old", Body: "I remember how this worked."})
				return err
			}
			if len(os.Args) == 6 {
				for _, item := range []struct{ id, kind, ask string }{
					{"inb_smoke_recovery", "recovery", `{"text":"Agent needs recovery"}`},
					{"inb_smoke_delivery", "delivery_failed", `{"text":"Agent reply was held"}`},
				} {
					if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,options_json,created_at)
					VALUES (?,?,?,'agent','agt_smoke',?,?,datetime('now'))`, item.id, os.Args[2], os.Args[3], item.kind, item.ask); err != nil {
						return err
					}
					if _, err := tx.AppendEvent(store.Event{Kind: "inbox_item_created", ProjectID: os.Args[2], RoomID: os.Args[3], PayloadJSON: fmt.Sprintf(`{"inbox_item_id":%q}`, item.id)}); err != nil {
						return err
					}
				}
				return nil
			}
			// Each question hangs off its own agent message, as an agent's attention question does: a person's reply in that message's thread
			// answers it (and the settle pass closes a question whose thread was already answered), so the two must not share the person's message.
			// They are raised by two different agents: one agent's newer card in a room supersedes its older one (the settle sweep applies this
			// on its own schedule), which would close the first question a moment after it was seeded.
			for _, item := range []struct{ id, agent, text, ask string }{
				{"inb_smoke_a", "agt_smoke", "Approve this choice?", `{"text":"Approve this choice?","options":["Approve","Revise"],"free_text":true}`},
				{"inb_smoke_b", "agt_smoke_b", "Explain the choice", `{"text":"Explain the choice","options":[],"free_text":true}`},
			} {
				ask, err := tx.InsertMessage(store.Message{RoomID: os.Args[3], AuthorKind: "agent", AuthorID: item.agent, AuthorHarness: "fake", AuthorModel: "test", Body: item.text})
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,requester_kind,requester_id,kind,options_json,created_at)
				VALUES (?,?,?,?,'agent',?,'question',?,datetime('now'))`, item.id, os.Args[2], os.Args[3], ask.ID, item.agent, item.ask); err != nil {
					return err
				}
				if _, err := tx.AppendEvent(store.Event{Kind: "inbox_item_created", ProjectID: os.Args[2], RoomID: os.Args[3], PayloadJSON: fmt.Sprintf(`{"inbox_item_id":%q}`, item.id)}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for {
		err = seed()
		var sqliteErr *sqlite.Error
		if err == nil || !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != 5 && sqliteErr.Code()&0xff != 6 {
			break
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(100 * time.Millisecond):
			continue
		}
		break
	}
	if err != nil {
		panic(err)
	}
}
