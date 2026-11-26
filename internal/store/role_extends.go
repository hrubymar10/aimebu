package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/goccy/go-json"
)

// A role may extend another role by key: the instructions an agent receives are the parent's resolved body, a blank line, then the
// role's own body.
var (
	// ErrRoleExtends rejects an extends value that names no role, points a role at itself, makes a cycle, or chains too deep.
	ErrRoleExtends = errors.New("invalid_extends")
	// ErrRoleExtended refuses to delete a role that other roles extend.
	ErrRoleExtended = errors.New("role_extended")
)

const maxRoleExtendsDepth = 5

type roleQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// allLiveRoles are every live role. A change to a role can reach any role that extends it, so it is checked against all of them.
func allLiveRoles(ctx context.Context, q roleQuerier) ([]RoleDefinition, error) {
	return queryRoles(ctx, q, ` WHERE deleted_at IS NULL`)
}

func queryRoles(ctx context.Context, q roleQuerier, where string, args ...any) ([]RoleDefinition, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,role_key,label,emoji,COALESCE(body,''),cardinality,
 cap_is_lead,cap_can_spawn,cap_singleton_writer,rev,extends,spawn_defaults_json,spawn_pick_mode FROM role`+where+` ORDER BY role_key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RoleDefinition{}
	defaults := builtinRoles()
	for rows.Next() {
		var r RoleDefinition
		var defaultsJSON string
		if err := rows.Scan(&r.ID, &r.Key, &r.Label, &r.Emoji, &r.Body, &r.Cardinality,
			&r.Lead, &r.CanSpawn, &r.Writer, &r.Rev, &r.Extends, &defaultsJSON, &r.PickMode); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(defaultsJSON), &r.DefaultAgents); err != nil {
			return nil, err
		}
		if r.DefaultAgents == nil {
			r.DefaultAgents = []SpawnDefaultChoice{}
		}
		shipped, builtin := defaults[r.ID]
		r.Builtin = builtin
		if builtin && r.Body == "" {
			r.Body = shipped.Body
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// roleParent finds the role that `from` extends, or reports that there is none. A role is never its own parent.
func roleParent(from RoleDefinition, all []RoleDefinition) (RoleDefinition, bool) {
	if from.Extends == "" {
		return RoleDefinition{}, false
	}
	for _, p := range all {
		if p.ID != from.ID && p.Key == from.Extends {
			return p, true
		}
	}
	return RoleDefinition{}, false
}

// resolveRoleBody is the text an agent with this role receives. A role with no parent (or whose parent is missing) gives its own body
// untouched; otherwise the chain, root first, joined by a blank line.
func resolveRoleBody(role RoleDefinition, all []RoleDefinition) string {
	parts := []string{role.Body}
	seen := map[string]bool{role.ID: true}
	cur := role
	for depth := 0; depth < maxRoleExtendsDepth; depth++ {
		parent, ok := roleParent(cur, all)
		if !ok || seen[parent.ID] {
			break
		}
		seen[parent.ID] = true
		parts = append(parts, parent.Body)
		cur = parent
	}
	if len(parts) == 1 {
		return role.Body
	}
	out := make([]string, 0, len(parts))
	for i := len(parts) - 1; i >= 0; i-- {
		if body := strings.TrimSpace(parts[i]); body != "" {
			out = append(out, body)
		}
	}
	return strings.Join(out, "\n\n")
}

// checkRoleExtends validates the candidate's extends against the roles it would live among: the target must exist, and following it
// must neither come back to the candidate nor run deeper than maxRoleExtendsDepth. A target further up the chain that has gone missing
// is not an error (resolution stops there).
func checkRoleExtends(candidate RoleDefinition, all []RoleDefinition) error {
	if candidate.Extends == "" {
		return nil
	}
	if !roleKeyPattern.MatchString(candidate.Extends) {
		return ErrRoleExtends
	}
	list := make([]RoleDefinition, 0, len(all)+1)
	replaced := false
	for _, r := range all {
		if candidate.ID != "" && r.ID == candidate.ID {
			list = append(list, candidate)
			replaced = true
		} else {
			list = append(list, r)
		}
	}
	if !replaced {
		list = append(list, candidate)
	}
	cur, steps := candidate, 0
	seen := map[string]bool{candidate.ID: true}
	for cur.Extends != "" {
		parent, ok := roleParent(cur, list)
		if !ok {
			if steps == 0 {
				return ErrRoleExtends
			}
			break
		}
		steps++
		// A revisit is a cycle even when it does not pass through the candidate (A -> B -> C -> B).
		if parent.ID == candidate.ID || seen[parent.ID] || steps > maxRoleExtendsDepth {
			return ErrRoleExtends
		}
		seen[parent.ID] = true
		cur = parent
	}
	// The change reaches roles that extend the candidate, directly or through others: a role further down must not end up with more
	// than maxRoleExtendsDepth ancestors, or its resolved body would silently lose the top of its chain.
	return checkChainDepths(list, candidate.ID)
}

// checkChainDepths is ErrRoleExtends when any role in the list (other than `skipID`) has more than maxRoleExtendsDepth ancestors.
func checkChainDepths(list []RoleDefinition, skipID string) error {
	for _, r := range list {
		if r.ID == skipID && skipID != "" || r.Extends == "" {
			continue
		}
		depth, walker := 0, r
		seen := map[string]bool{r.ID: true}
		for walker.Extends != "" {
			parent, ok := roleParent(walker, list)
			if !ok {
				break
			}
			depth++
			if depth > maxRoleExtendsDepth || seen[parent.ID] {
				return ErrRoleExtends
			}
			seen[parent.ID] = true
			walker = parent
		}
	}
	return nil
}

// effectiveRoleBody is the resolved body of a role by id. A role that has since been deleted still answers with its own stored body,
// since an agent can outlive its role definition.
func (tx *Tx) effectiveRoleBody(roleID string) (string, error) {
	all, err := allLiveRoles(tx.ctx, tx.tx)
	if err != nil {
		return "", err
	}
	for _, r := range all {
		if r.ID == roleID {
			return resolveRoleBody(r, all), nil
		}
	}
	var body string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(body,'') FROM role WHERE id=?`, roleID).Scan(&body)
	return body, err
}

// roleExtendedBy reports whether deleting the role would strand others that extend its key.
func (tx *Tx) roleExtendedBy(role RoleDefinition) (bool, error) {
	var count int
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM role WHERE deleted_at IS NULL AND id<>? AND extends=?`, role.ID, role.Key).Scan(&count)
	return count > 0, err
}

// ResetRole puts a built-in global role back to what shipped: its name, emoji, instructions, parent, cardinality and capabilities.
// Only the instance owner or an admin may, as for any change to a global role's capabilities, and not while live agents hold the role with
// capabilities or cardinality that differ from the shipped ones. The revision moves on, so a stale editor is told.
func (s *Store) ResetRole(ctx context.Context, actorID, id string, rev int64) (RoleDefinition, error) {
	var result RoleDefinition
	err := s.Tx(ctx, func(tx *Tx) error {
		if err := tx.authorizeDefinitionActor("", actorID, true); err != nil {
			return err
		}
		def, ok := builtinRoles()[id]
		if !ok {
			return ErrInvalidTransition
		}
		all, err := allLiveRoles(tx.ctx, tx.tx)
		if err != nil {
			return err
		}
		var old *RoleDefinition
		for i := range all {
			if all[i].ID == id {
				old = &all[i]
			}
		}
		if old == nil {
			return sql.ErrNoRows
		}
		if old.Rev != rev {
			return ErrStaleRev
		}
		reset := *old
		reset.Label, reset.Emoji, reset.Body, reset.Cardinality, reset.Extends = def.Label, def.Emoji, def.Body, def.Cardinality, def.Extends
		reset.Lead, reset.CanSpawn, reset.Writer = def.Lead, def.CanSpawn, def.Writer
		if err := checkRoleExtends(reset, all); err != nil {
			return err
		}
		if reset.Lead != old.Lead || reset.CanSpawn != old.CanSpawn || reset.Writer != old.Writer || reset.Cardinality != old.Cardinality {
			var live int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent WHERE role_id=? AND revoked_at IS NULL
    AND (desired!='stopped' OR observed!='stopped')`, id).Scan(&live); err != nil {
				return err
			}
			if live > 0 {
				return ErrRoleInUse
			}
		}
		if _, err := tx.Exec(`UPDATE role SET label=?,emoji=?,body=?,extends=?,cardinality=?,cap_is_lead=?,
   cap_can_spawn=?,cap_singleton_writer=?,spawn_defaults_json='[]',spawn_pick_mode='in_turn',rev=rev+1 WHERE id=?`,
			reset.Label, reset.Emoji, "", reset.Extends, reset.Cardinality, reset.Lead, reset.CanSpawn, reset.Writer, id); err != nil {
			return err
		}
		reset.DefaultAgents, reset.PickMode = []SpawnDefaultChoice{}, "in_turn"
		if err := tx.syncPoolChanges(); err != nil {
			return err
		}
		reset.Rev++
		for i := range all {
			if all[i].ID == id {
				all[i] = reset
			}
		}
		reset.ResolvedBody = resolveRoleBody(reset, all)
		result = reset
		return nil
	})
	return result, err
}
