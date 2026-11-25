package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-json"
)

var roleKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)
var ErrRoleInUse = errors.New("role_in_use")

func (tx *Tx) AuthorizeSpawn(roomID, actorKind, actorID string) error {
	var projectID, lifecycle string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,lifecycle FROM room WHERE id=?`, roomID).
		Scan(&projectID, &lifecycle); err != nil {
		return err
	}
	if lifecycle != "open" {
		return ErrInvalidTransition
	}
	return tx.authorizeSpawnActor(projectID, actorKind, actorID, true)
}

type RoleDefinition struct {
	ID            string               `json:"id"`
	Key           string               `json:"role_key"`
	Label         string               `json:"label"`
	Emoji         string               `json:"emoji"`
	Body          string               `json:"body"`
	Cardinality   string               `json:"cardinality"`
	Lead          bool                 `json:"cap_is_lead"`
	CanSpawn      bool                 `json:"cap_can_spawn"`
	Writer        bool                 `json:"cap_singleton_writer"`
	DefaultAgents []SpawnDefaultChoice `json:"default_agents"`
	PickMode      string               `json:"pick_mode"`
	Rev           int64                `json:"rev"`
	Builtin       bool                 `json:"builtin"`
	// Extends is the key of the role this one adds to ("" for none); ResolvedBody, set only when listing, is the text an agent
	// with the role receives (see role_extends.go).
	Extends      string `json:"extends"`
	ResolvedBody string `json:"resolved_body,omitempty"`
	// BodyExtension is the role's global prompt extension. On the list it is set only when one is saved; on a write a nil
	// pointer keeps what is saved and a set pointer replaces or clears it (an owner-only field, see role_body_extension.go).
	BodyExtension *string `json:"body_extension,omitempty"`
	// EffectiveBody, set only when listing, is the delivered text at the global scope: the resolved body with the global
	// extension joined, skipping empty parts. A project's flow override and extension add to what its agents receive.
	EffectiveBody string `json:"effective_body"`
}

func (s *Store) ListRoles(ctx context.Context) ([]RoleDefinition, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	roles, err := allLiveRoles(ctx, s.db)
	if err != nil {
		return nil, err
	}
	extensions, err := globalRoleBodyExtensions(ctx, s.db)
	if err != nil {
		return nil, err
	}
	for i := range roles {
		roles[i].ResolvedBody = resolveRoleBody(roles[i], roles)
		if ext, ok := extensions[roles[i].ID]; ok {
			copy := ext.Body
			roles[i].BodyExtension = &copy
		}
		var globalExt string
		if roles[i].BodyExtension != nil {
			globalExt = *roles[i].BodyExtension
		}
		roles[i].EffectiveBody = joinBodyParts(roles[i].ResolvedBody, globalExt)
	}
	return roles, nil
}

// ErrRoleNameCollision rejects a new role key that equals the slug of a live
// agent: @key would then address the role instead of, or as well as, the agent.
var ErrRoleNameCollision = errors.New("role key collides with an agent or human name")

func (s *Store) ManageRole(ctx context.Context, actorID string, r RoleDefinition, deleteRole bool) (RoleDefinition, error) {
	var result RoleDefinition
	err := s.Tx(ctx, func(tx *Tx) error {
		if err := tx.authorizeDefinitionActor("", actorID, false); err != nil {
			return err
		}
		existing := r.ID != ""
		if deleteRole || r.ID != "" {
			var old RoleDefinition
			var defaultsJSON string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,role_key,label,emoji,COALESCE(body,''),cardinality,
   cap_is_lead,cap_can_spawn,cap_singleton_writer,rev,spawn_defaults_json,spawn_pick_mode FROM role WHERE id=? AND deleted_at IS NULL`, r.ID).
				Scan(&old.ID, &old.Key, &old.Label, &old.Emoji, &old.Body, &old.Cardinality,
					&old.Lead, &old.CanSpawn, &old.Writer, &old.Rev, &defaultsJSON, &old.PickMode)
			if err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(defaultsJSON), &old.DefaultAgents); err != nil {
				return err
			}
			if old.Rev != r.Rev {
				return ErrStaleRev
			}
			if deleteRole {
				if _, builtin := builtinRoles()[old.ID]; builtin {
					return ErrForbidden
				}
				var count int
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent WHERE role_id=?
					AND (desired!='stopped' OR observed!='stopped')`, old.ID).Scan(&count); err != nil {
					return err
				}
				if count > 0 {
					return ErrRoleInUse
				}
				extended, err := tx.roleExtendedBy(old)
				if err != nil {
					return err
				}
				if extended {
					return ErrRoleExtended
				}
				if _, err := tx.Exec(`DELETE FROM role_body_extension WHERE role_id=?`, old.ID); err != nil {
					return err
				}
				_, err = tx.Exec(`UPDATE role SET deleted_at=?,key='deleted:'||id,rev=rev+1 WHERE id=?`, timestamp(), old.ID)
				return err
			}
			// Older callers omit these fields. An omitted list keeps the current
			// defaults; an explicit [] clears them.
			if r.DefaultAgents == nil {
				r.DefaultAgents = old.DefaultAgents
			}
			if r.PickMode == "" {
				r.PickMode = old.PickMode
			}
			if r.Key != old.Key {
				return ErrInvalidTransition
			}
			if r.Lead != old.Lead || r.CanSpawn != old.CanSpawn || r.Writer != old.Writer {
				if err := tx.authorizeDefinitionActor("", actorID, true); err != nil {
					return err
				}
			}
			if r.Lead != old.Lead || r.CanSpawn != old.CanSpawn || r.Writer != old.Writer || r.Cardinality != old.Cardinality {
				var live int
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent WHERE role_id=? AND revoked_at IS NULL
    AND (desired!='stopped' OR observed!='stopped')`, r.ID).Scan(&live); err != nil {
					return err
				}
				if live > 0 {
					return ErrRoleInUse
				}
			}
		} else {
			if !roleKeyPattern.MatchString(r.Key) {
				return fmt.Errorf("%w: role_key", ErrInvalidTransition)
			}
			var count int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM role WHERE role_key=? AND deleted_at IS NULL`, r.Key).Scan(&count); err != nil {
				return err
			}
			if count > 0 {
				return ErrConflict
			}
			// A role key is a live @mention, so it must not shadow an agent
			// that is addressable under the same slug (nor, below, a human
			// login). Only live agents hold
			// a name: stopped and revoked ones do not, so deleting a role
			// still frees its key for an agent that was stopped under it.
			var collides int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent WHERE lower(slug)=lower(?) AND revoked_at IS NULL
				AND (desired!='stopped' OR observed!='stopped')`, r.Key).Scan(&collides); err != nil {
				return err
			}
			if collides > 0 {
				return ErrRoleNameCollision
			}
			// Humans are addressable by login, and an exact login wins over a
			// role key, so a role named like an active human would be dead
			// weight: @key would reach the person and never the role holders.
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM user u WHERE lower(u.login)=lower(?) AND u.disabled_at IS NULL`, r.Key).Scan(&collides); err != nil {
				return err
			}
			if collides > 0 {
				return ErrRoleNameCollision
			}
			if r.Lead || r.CanSpawn || r.Writer {
				if err := tx.authorizeDefinitionActor("", actorID, true); err != nil {
					return err
				}
			}
			var err error
			r.ID, err = newID("role")
			if err != nil {
				return err
			}
		}
		if strings.TrimSpace(r.Label) == "" || len(r.Label) > 80 || len(r.Body) > 32768 || len(r.Emoji) > 32 ||
			(r.Cardinality != "multi" && r.Cardinality != "singleton") {
			return ErrInvalidTransition
		}
		if r.PickMode == "" {
			r.PickMode = "in_turn"
		}
		if err := tx.validateSpawnDefaults(r.DefaultAgents, r.PickMode); err != nil {
			return err
		}
		defaultsJSON, err := json.Marshal(r.DefaultAgents)
		if err != nil {
			return err
		}
		visible, err := allLiveRoles(tx.ctx, tx.tx)
		if err != nil {
			return err
		}
		if err := checkRoleExtends(r, visible); err != nil {
			return err
		}
		storedBody := r.Body
		if def, ok := builtinRoles()[r.ID]; ok && (storedBody == "" || storedBody == def.Body) {
			storedBody = ""
			r.Body = def.Body
		}
		if existing {
			_, err := tx.Exec(`UPDATE role SET label=?,emoji=?,body=?,extends=?,cardinality=?,cap_is_lead=?,
   cap_can_spawn=?,cap_singleton_writer=?,spawn_defaults_json=?,spawn_pick_mode=?,rev=rev+1 WHERE id=?`,
				r.Label, r.Emoji, storedBody, r.Extends, r.Cardinality, r.Lead, r.CanSpawn, r.Writer, string(defaultsJSON), r.PickMode, r.ID)
			if err != nil {
				return err
			}
			r.Rev++
		} else {
			_, err := tx.Exec(`INSERT INTO role(id,key,project_id,role_key,label,emoji,body,extends,cardinality,
   cap_is_lead,cap_can_spawn,cap_singleton_writer,spawn_defaults_json,spawn_pick_mode)
   VALUES (?,?,NULL,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.Key, r.Key, r.Label, r.Emoji,
				r.Body, r.Extends, r.Cardinality, r.Lead, r.CanSpawn, r.Writer, string(defaultsJSON), r.PickMode)
			if err != nil {
				return err
			}
		}
		result = r
		// The extension rides the same guarded write: replacing or clearing it needs the instance owner, and a caller that
		// leaves the field out keeps what is saved.
		if r.BodyExtension != nil {
			level, err := tx.privilege(actorID)
			if err != nil {
				return err
			}
			if level < privOwner {
				return ErrForbidden
			}
			if _, err := tx.saveRoleBodyExtension(r.ID, "", *r.BodyExtension, 0, true); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (tx *Tx) authorizeDefinitionActor(projectID, actorID string, capabilityChange bool) error {
	if projectID == "" {
		// Global roles and prompts are global settings: the owner's and the admins'.
		level, err := tx.privilege(actorID)
		if err != nil {
			return err
		}
		if level < privAdmin {
			return ErrForbidden
		}
		return nil
	}
	var role string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, actorID).Scan(&role)
	if err != nil {
		return err
	}
	if role == "owner" || role == "operator" && !capabilityChange {
		return nil
	}
	return ErrForbidden
}
