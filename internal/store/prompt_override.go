package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/hrubymar10/aimebu/v2/internal/adapter/prompt"
)

// Prompt overrides replace the shared prose sentences of an adapter's turn prompt (prompt.Slots): one row per scope and slot, global
// (project_id NULL) or for one project. A project's row wins over the global one, which wins over the shipped text. The output
// contract and the adapter-specific parts are not slots, so an override cannot break how a turn's answer is read.

// ErrPromptOverride is a rejected override: an unknown slot, or a value that is empty or longer than prompt.MaxOverrideLen.
var ErrPromptOverride = errors.New("invalid prompt override")

// Inline plans: whether a lead's instructions always ask for the full plan as appendix pages with a visual plan, or leave it to the lead.
const (
	InlinePlanAppendixKey = "inline_plan_appendix"
	InlinePlanOptional    = "optional"
	InlinePlanAlways      = "always"
	// inlinePlanAlwaysText is added to a lead's instructions when the setting is "always".
	inlinePlanAlwaysText = `When you send a visual_plan, always attach the full prose as appendix_pages, rendered as a collapsed "Full plan" block at the visual_plan tail; do not omit it, and do not duplicate proceed/answer controls inside it.`
)

// PromptOverride is one saved override.
type PromptOverride struct {
	Value     string `json:"value"`
	Rev       int64  `json:"rev"`
	UpdatedAt string `json:"updated_at"`
}

// PromptSlot is a slot with its shipped text, what is saved at each scope and what a turn in the viewed scope would get.
type PromptSlot struct {
	prompt.Slot
	Max       int             `json:"max"`
	Global    *PromptOverride `json:"global,omitempty"`
	Project   *PromptOverride `json:"project,omitempty"`
	Effective string          `json:"effective"`
	Source    string          `json:"source"` // "default", "global" or "project"
}

type promptQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// promptRows reads the saved overrides of one scope (empty projectID is global), by slot key.
func promptRows(ctx context.Context, db promptQuerier, projectID string) (map[string]PromptOverride, error) {
	rows, err := db.QueryContext(ctx, `SELECT key,value,rev,updated_at FROM prompt_override WHERE COALESCE(project_id,'')=?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PromptOverride{}
	for rows.Next() {
		var key string
		var o PromptOverride
		if err := rows.Scan(&key, &o.Value, &o.Rev, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out[key] = o
	}
	return out, rows.Err()
}

// ListPrompts returns every slot as seen from a scope: the global overrides, and when projectID is set that project's too.
func (s *Store) ListPrompts(ctx context.Context, projectID string) ([]PromptSlot, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	global, err := promptRows(ctx, s.db, "")
	if err != nil {
		return nil, err
	}
	var project map[string]PromptOverride
	if projectID != "" {
		if project, err = promptRows(ctx, s.db, projectID); err != nil {
			return nil, err
		}
	}
	slots := []PromptSlot{}
	for _, slot := range prompt.Slots() {
		view := PromptSlot{Slot: slot, Max: prompt.MaxOverrideLen, Effective: slot.Default, Source: "default"}
		if o, ok := global[slot.Key]; ok {
			copy := o
			view.Global, view.Effective, view.Source = &copy, o.Value, "global"
		}
		if o, ok := project[slot.Key]; ok {
			copy := o
			view.Project, view.Effective, view.Source = &copy, o.Value, "project"
		}
		slots = append(slots, view)
	}
	return slots, nil
}

// SetPromptOverride saves one override. rev is 0 to create (it must not exist yet) or the current revision to change it. Global
// overrides are the instance owner's or an admin's; a project's are its owner's or operators'.
func (s *Store) SetPromptOverride(ctx context.Context, actorID, projectID, key, value string, rev int64) error {
	value = strings.TrimSpace(value)
	if !prompt.ValidSlot(key) || value == "" || utf8.RuneCountInString(value) > prompt.MaxOverrideLen {
		return ErrPromptOverride
	}
	return s.Tx(ctx, func(tx *Tx) error {
		if err := tx.authorizeDefinitionActor(projectID, actorID, projectID == ""); err != nil {
			return err
		}
		var current int64
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT rev FROM prompt_override WHERE COALESCE(project_id,'')=? AND key=?`, projectID, key).Scan(&current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if rev != 0 {
				return ErrStaleRev
			}
			id, err := newID("pov")
			if err != nil {
				return err
			}
			var project any
			if projectID != "" {
				project = projectID
			}
			_, err = tx.Exec(`INSERT INTO prompt_override(id,project_id,key,value,rev,updated_at) VALUES (?,?,?,?,1,?)`, id, project, key, value, timestamp())
			return err
		case err != nil:
			return err
		case current != rev:
			return ErrStaleRev
		}
		_, err = tx.Exec(`UPDATE prompt_override SET value=?,rev=rev+1,updated_at=? WHERE COALESCE(project_id,'')=? AND key=?`, value, timestamp(), projectID, key)
		return err
	})
}

// ResetPromptOverride removes one override, so the next scope down (or the shipped text) applies again.
func (s *Store) ResetPromptOverride(ctx context.Context, actorID, projectID, key string, rev int64) error {
	if !prompt.ValidSlot(key) {
		return ErrPromptOverride
	}
	return s.Tx(ctx, func(tx *Tx) error {
		if err := tx.authorizeDefinitionActor(projectID, actorID, projectID == ""); err != nil {
			return err
		}
		var current int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT rev FROM prompt_override WHERE COALESCE(project_id,'')=? AND key=?`, projectID, key).Scan(&current); err != nil {
			return err // sql.ErrNoRows reads as not found
		}
		if current != rev {
			return ErrStaleRev
		}
		_, err := tx.Exec(`DELETE FROM prompt_override WHERE COALESCE(project_id,'')=? AND key=?`, projectID, key)
		return err
	})
}

// promptOverrides is what a turn of this project is given: the global overrides with the project's laid over them. Nil when none.
func (tx *Tx) promptOverrides(projectID string) (map[string]string, error) {
	out := map[string]string{}
	scopes := []string{""}
	if projectID != "" {
		scopes = append(scopes, projectID)
	}
	for _, scope := range scopes {
		rows, err := promptRows(tx.ctx, tx.tx, scope)
		if err != nil {
			return nil, err
		}
		for key, o := range rows {
			if prompt.ValidSlot(key) {
				out[key] = o.Value
			}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// InlinePlanAppendix is the global setting; "optional" (the v2 behaviour before the setting existed) when none was saved or a value
// is unreadable.
func (s *Store) InlinePlanAppendix(ctx context.Context) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM global_setting WHERE key=?`, InlinePlanAppendixKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || err == nil && value != InlinePlanAlways {
		return InlinePlanOptional, nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// SetInlinePlanAppendix saves the global setting (instance owner or an admin).
func (s *Store) SetInlinePlanAppendix(ctx context.Context, actorID, value string) error {
	if value != InlinePlanAlways && value != InlinePlanOptional {
		return ErrPromptOverride
	}
	return s.Tx(ctx, func(tx *Tx) error {
		if err := tx.authorizeDefinitionActor("", actorID, true); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO global_setting(key,value,updated_at) VALUES (?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, InlinePlanAppendixKey, value, timestamp())
		return err
	})
}

// withInlinePlan adds the "always attach the full plan" sentence to a lead's instructions when the global setting asks for it.
func (tx *Tx) withInlinePlan(roleID, body string) (string, error) {
	var lead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT cap_is_lead FROM role WHERE id=?`, roleID).Scan(&lead); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return body, nil
		}
		return "", err
	}
	if !lead {
		return body, nil
	}
	var value string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT value FROM global_setting WHERE key=?`, InlinePlanAppendixKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || err == nil && value != InlinePlanAlways {
		return body, nil
	}
	if err != nil {
		return "", err
	}
	return body + "\n\n" + inlinePlanAlwaysText, nil
}
