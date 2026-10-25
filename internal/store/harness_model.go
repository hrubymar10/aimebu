package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	harnessModelsMax     = 64
	harnessModelLabelMax = 80
	harnessModelNoteMax  = 280
)

var harnessModelSlugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

// HarnessModelFamilies are the harness families whose models the owner edits. The fake test adapter has no harness.
var HarnessModelFamilies = []string{"claude", "codex", "pi", "opencode", "vibe"}

// ErrInvalidHarnessModels wraps the reason a model list was refused.
var ErrInvalidHarnessModels = errors.New("invalid_harness_models")

// HarnessModel is one model a harness offers to spawn. The slug is the exact id the harness is started with, never an alias; the
// note is free text for the owner (a context size, a price class). Exactly one model of a non-empty list is the default, which is
// what an agent whose model was deleted falls back to.
type HarnessModel struct {
	Slug    string `json:"slug"`
	Label   string `json:"label"`
	Note    string `json:"note"`
	Default bool   `json:"default"`
}

// ErrStaleHarnessModels is a replace that was based on a list which has changed since.
var ErrStaleHarnessModels = errors.New("stale_harness_models")

// HarnessModelsRev names one version of a harness's list: a short digest of every model's id, label, note and default flag in order. A
// client that read a list keeps its revision and sends it back with its edit, so an edit based on a list someone else has changed in the
// meantime is refused instead of overwriting that change.
func HarnessModelsRev(models []HarnessModel) string {
	digest := sha256.New()
	for _, model := range models {
		fmt.Fprintf(digest, "%d:%s|%d:%s|%d:%s|%t\n", len(model.Slug), model.Slug, len(model.Label), model.Label, len(model.Note), model.Note, model.Default)
	}
	return hex.EncodeToString(digest.Sum(nil))[:16]
}

// ValidateHarnessModels checks a harness's complete list. An empty list is allowed: the harness then offers nothing to spawn until a
// model is added.
func ValidateHarnessModels(family string, models []HarnessModel) error {
	known := false
	for _, name := range HarnessModelFamilies {
		known = known || name == family
	}
	if !known {
		return fmt.Errorf("unknown harness family %q", family)
	}
	if len(models) > harnessModelsMax {
		return fmt.Errorf("at most %d models are allowed per harness", harnessModelsMax)
	}
	seen := map[string]bool{}
	defaults := 0
	for _, model := range models {
		if family == "opencode" && !strings.Contains(model.Slug, "/") {
			return fmt.Errorf("OpenCode model %q must be provider/model", model.Slug)
		}
		if !harnessModelSlugPattern.MatchString(model.Slug) {
			return fmt.Errorf("invalid model id %q", model.Slug)
		}
		if seen[model.Slug] {
			return fmt.Errorf("duplicate model id %q", model.Slug)
		}
		seen[model.Slug] = true
		if model.Label == "" || utf8.RuneCountInString(model.Label) > harnessModelLabelMax {
			return fmt.Errorf("model %q needs a label of at most %d characters", model.Slug, harnessModelLabelMax)
		}
		if utf8.RuneCountInString(model.Note) > harnessModelNoteMax {
			return fmt.Errorf("the note of model %q is longer than %d characters", model.Slug, harnessModelNoteMax)
		}
		if model.Default {
			defaults++
		}
	}
	if len(models) > 0 && defaults != 1 {
		return fmt.Errorf("mark exactly one model as the default (%d are)", defaults)
	}
	return nil
}

// ValidHarnessModelSlug says whether a model id is acceptable as a command-line value.
func ValidHarnessModelSlug(slug string) bool { return harnessModelSlugPattern.MatchString(slug) }

// HarnessModels returns a harness's models in the owner's order.
func (s *Store) HarnessModels(ctx context.Context, launcherID string) ([]HarnessModel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slug,label,note,is_default FROM harness_model WHERE launcher_id=? ORDER BY position`, launcherID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := []HarnessModel{}
	for rows.Next() {
		var model HarnessModel
		if err := rows.Scan(&model.Slug, &model.Label, &model.Note, &model.Default); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, rows.Err()
}

// MachineHarnessModels returns the models of every harness of a machine, by launcher id.
func (s *Store) MachineHarnessModels(ctx context.Context, machineID string) (map[string][]HarnessModel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.launcher_id,m.slug,m.label,m.note,m.is_default FROM harness_model m JOIN machine_launcher l ON l.id=m.launcher_id
		WHERE l.machine_id=? ORDER BY m.launcher_id,m.position`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := map[string][]HarnessModel{}
	for rows.Next() {
		var id string
		var model HarnessModel
		if err := rows.Scan(&id, &model.Slug, &model.Label, &model.Note, &model.Default); err != nil {
			return nil, err
		}
		models[id] = append(models[id], model)
	}
	return models, rows.Err()
}

// ReplaceHarnessModels replaces a harness's whole list atomically and records the change in the machine's launcher activity. The list is
// validated against the harness's family; sql.ErrNoRows when the harness is not on that machine.
func (s *Store) ReplaceHarnessModels(ctx context.Context, actor, machineID, launcherID string, models []HarnessModel) error {
	return s.ReplaceHarnessModelsIfRev(ctx, actor, machineID, launcherID, models, "")
}

// ReplaceHarnessModelsIfRev is ReplaceHarnessModels for an edit based on the list with revision expectedRev (see HarnessModelsRev): when the
// stored list is no longer that one it changes nothing and answers ErrStaleHarnessModels. An empty expectedRev replaces unconditionally.
func (s *Store) ReplaceHarnessModelsIfRev(ctx context.Context, actor, machineID, launcherID string, models []HarnessModel, expectedRev string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var family string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT tool FROM machine_launcher WHERE id=? AND machine_id=?`, launcherID, machineID).Scan(&family); err != nil {
			return err
		}
		if expectedRev != "" {
			current, err := tx.harnessModelList(launcherID)
			if err != nil {
				return err
			}
			if HarnessModelsRev(current) != expectedRev {
				return ErrStaleHarnessModels
			}
		}
		if err := ValidateHarnessModels(family, models); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidHarnessModels, err.Error())
		}
		before, err := tx.harnessModelSlugs(launcherID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM harness_model WHERE launcher_id=?`, launcherID); err != nil {
			return err
		}
		for i, model := range models {
			if _, err := tx.Exec(`INSERT INTO harness_model(launcher_id,slug,label,note,is_default,position) VALUES (?,?,?,?,?,?)`, launcherID, model.Slug, model.Label, model.Note, model.Default, i); err != nil {
				return err
			}
		}
		id, err := newID("mla")
		if err != nil {
			return err
		}
		after := make([]string, 0, len(models))
		for _, model := range models {
			after = append(after, model.Slug)
		}
		_, err = tx.Exec(`INSERT INTO machine_launcher_activity(id,machine_id,launcher_id,actor,action,before_json,after_json,created_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, machineID, launcherID, actor, "models", before, jsonList(after), timestamp())
		return err
	})
}

func (tx *Tx) harnessModelList(launcherID string) ([]HarnessModel, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT slug,label,note,is_default FROM harness_model WHERE launcher_id=? ORDER BY position`, launcherID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := []HarnessModel{}
	for rows.Next() {
		var model HarnessModel
		if err := rows.Scan(&model.Slug, &model.Label, &model.Note, &model.Default); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, rows.Err()
}

func (tx *Tx) harnessModelSlugs(launcherID string) (string, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT slug FROM harness_model WHERE launcher_id=? ORDER BY position`, launcherID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return "", err
		}
		slugs = append(slugs, slug)
	}
	return jsonList(slugs), rows.Err()
}

// HarnessModelFor says which model a harness starts an agent with: the one asked for when the harness lists it, otherwise the
// harness's default, so an agent whose model the owner deleted switches to the default on its next start. replaced is true when the
// answer is not the model asked for. An empty list gives "" and false.
func (s *Store) HarnessModelFor(ctx context.Context, launcherID, wanted string) (model string, replaced bool, err error) {
	var def string
	err = s.db.QueryRowContext(ctx, `SELECT slug FROM harness_model WHERE launcher_id=? AND is_default=1`, launcherID).Scan(&def)
	if err == sql.ErrNoRows {
		return wanted, false, nil
	}
	if err != nil {
		return "", false, err
	}
	var listed int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_model WHERE launcher_id=? AND slug=?`, launcherID, wanted).Scan(&listed); err != nil {
		return "", false, err
	}
	if listed > 0 {
		return wanted, false, nil
	}
	return def, true, nil
}

// settleAgentModel is HarnessModelFor inside a transaction, for an agent about to start: when the harness no longer lists the agent's
// model it is switched to the harness default, the agent row is updated so the card shows it, and the change goes in the machine's
// launcher activity as "model_default". An agent without a harness, or a harness with no list, is left as it is.
func (tx *Tx) settleAgentModel(agentID, launcherID, model string) (string, error) {
	if launcherID == "" {
		return model, nil
	}
	var def string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT slug FROM harness_model WHERE launcher_id=? AND is_default=1`, launcherID).Scan(&def); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model, nil
		}
		return "", err
	}
	var listed int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM harness_model WHERE launcher_id=? AND slug=?`, launcherID, model).Scan(&listed); err != nil {
		return "", err
	}
	if listed > 0 {
		return model, nil
	}
	var machineID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT machine_id FROM machine_launcher WHERE id=?`, launcherID).Scan(&machineID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE agent SET model=? WHERE id=?`, def, agentID); err != nil {
		return "", err
	}
	id, err := newID("mla")
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO machine_launcher_activity(id,machine_id,launcher_id,actor,action,before_json,after_json,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		id, machineID, launcherID, agentID, "model_default", jsonList([]string{model}), jsonList([]string{def}), timestamp())
	return def, err
}

// settlePendingModels applies settleAgentModel to agents that are about to be spawned.
func (s *Store) settlePendingModels(ctx context.Context, pending []PendingSpawn) error {
	for i := range pending {
		if pending[i].LauncherID == "" {
			continue
		}
		err := s.Tx(ctx, func(tx *Tx) error {
			model, err := tx.settleAgentModel(pending[i].AgentID, pending[i].LauncherID, pending[i].Model)
			pending[i].Model = model
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
