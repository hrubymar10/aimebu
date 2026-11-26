package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type V1PreviewRole struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Emoji       string `json:"emoji"`
	Body        string `json:"body"`
	Cardinality string `json:"cardinality"`
	// Extends is the parent the role keeps as a link (its Body is then only its own text); FlattenedFrom is set instead when the
	// parent is not available in v2 and so the role could not keep the link.
	Extends        string `json:"extends,omitempty"`
	FlattenedFrom  string `json:"flattened_from,omitempty"`
	SkippedReason  string `json:"skipped_reason,omitempty"`
	Conflict       bool   `json:"conflict"`
	CurrentBody    string `json:"current_body,omitempty"`
	CurrentExtends string `json:"current_extends,omitempty"`
}

var v1BuiltinParents = map[string]string{
	"sec-reviewer":  "reviewer",
	"test-reviewer": "reviewer",
	"ux-reviewer":   "reviewer",
}

func destinationRoleMap(roles []RoleDefinition) map[string]RoleDefinition {
	result := make(map[string]RoleDefinition, len(roles))
	for _, role := range roles {
		result[role.Key] = role
	}
	return result
}

// resolveV1Roles turns the v1 role definitions into what v2 will hold, deciding every role's final state before any link is set, so a
// link only ever points at a role that will really exist: one v2 has live now, or one this run imports. A source role is not
// imported by this run when it is ruled out (key, name, cardinality, a loop, a missing or oversized body) or when `ledgered` says an
// earlier run already brought it (it is then whatever v2 has now, which may be nothing if the owner deleted it). A role keeps a
// link to its parent when the parent will exist, the chain stays within maxRoleExtendsDepth ancestors and its own text fits;
// otherwise it is flattened from the v1 text as before (FlattenedFrom names the parent it could not keep), or skipped when even
// that is too long. The preview and the apply both come from here, so they agree.
func resolveV1Roles(source map[string]v1ImportRole, destination map[string]RoleDefinition, ledgered map[string]bool) []V1PreviewRole {
	keys := sortedImportKeys(source)
	result := make([]V1PreviewRole, 0, len(keys))
	destList := make([]RoleDefinition, 0, len(destination))
	for _, role := range destination {
		destList = append(destList, role)
	}
	parentOf := func(key string) string {
		spec := source[key]
		parentKey := spec.Extends
		if parentKey == "" && !spec.Custom && spec.Body != "" {
			parentKey = v1BuiltinParents[key]
		}
		// A role with no body of its own in v1 takes v2's text, and with it v2's parent.
		if parentKey == "" && strings.TrimSpace(spec.Body) == "" {
			parentKey = destination[key].Extends
		}
		return parentKey
	}
	var bodyFor func(string, map[string]bool) (string, error)
	bodyFor = func(key string, seen map[string]bool) (string, error) {
		if seen[key] {
			return "", fmt.Errorf("inheritance cycle at %s", key)
		}
		seen[key] = true
		defer delete(seen, key)
		spec, fromSource := source[key]
		body := strings.TrimSpace(spec.Body)
		if body == "" {
			body = strings.TrimSpace(destination[key].Body)
		}
		if body == "" {
			return "", fmt.Errorf("missing parent or body %s", key)
		}
		parentKey := parentOf(key)
		if !fromSource {
			// A role only v2 has contributes its whole effective text, parent included.
			if dest, ok := destination[key]; ok {
				return strings.TrimSpace(resolveRoleBody(dest, destList)), nil
			}
			return body, nil
		}
		if parentKey == "" {
			return body, nil
		}
		parent, err := bodyFor(parentKey, seen)
		if err != nil {
			return "", err
		}
		return parent + "\n\n" + body, nil
	}
	// draft is a source role as v2 would take it before parents are considered: its name, emoji and cardinality (v2's own filling in
	// what v1 left out) and whether those, or its key, rule it out.
	draft := func(key string) V1PreviewRole {
		spec := source[key]
		role := V1PreviewRole{Key: key, Label: strings.TrimSpace(spec.Description), Emoji: spec.Emoji, Cardinality: spec.Cardinality}
		if role.Label == "" {
			role.Label = destination[key].Label
		}
		if role.Label == "" {
			role.Label = key
		}
		if role.Emoji == "" {
			role.Emoji = spec.Icon
		}
		if role.Emoji == "" {
			role.Emoji = destination[key].Emoji
		}
		if role.Cardinality == "" {
			role.Cardinality = destination[key].Cardinality
		}
		if role.Cardinality == "" {
			role.Cardinality = "multi"
		}
		_, role.Conflict = destination[key]
		if role.Conflict {
			role.CurrentBody = destination[key].Body
			role.CurrentExtends = destination[key].Extends
		}
		if !roleKeyPattern.MatchString(key) {
			role.SkippedReason = "invalid v2 role key"
		}
		if role.Cardinality != "multi" && role.Cardinality != "singleton" {
			role.SkippedReason = "invalid v2 cardinality"
		}
		if len(role.Emoji) > 32 || len(role.Label) > 80 {
			role.SkippedReason = "role metadata exceeds v2 limits"
		}
		return role
	}
	ownBody := func(key string) string {
		if own := strings.TrimSpace(source[key].Body); own != "" {
			return own
		}
		return strings.TrimSpace(destination[key].Body)
	}
	type outcome struct {
		role     V1PreviewRole
		imported bool // this run writes it (not skipped, not already brought by an earlier run)
	}
	decided := map[string]*outcome{}
	var decide func(string) *outcome
	var ancestors func(string, int) int
	// destChain is the number of ancestors the destination's own role has.
	destChain := func(key string) int {
		n := 0
		for cur, ok := destination[key]; ok && cur.Extends != "" && n < maxRoleExtendsDepth+1; cur, ok = destination[cur.Extends] {
			n++
		}
		return n
	}
	// ancestors is how many ancestors a role would have after the import: the larger of what v2 has now and what the import brings.
	ancestors = func(key string, guard int) int {
		best := destChain(key)
		if _, inSource := source[key]; inSource && !ledgered[key] && guard < maxRoleExtendsDepth+2 {
			if o := decide(key); o.imported && o.role.Extends != "" {
				if n := 1 + ancestors(o.role.Extends, guard+1); n > best {
					best = n
				}
			}
		}
		return best
	}
	decide = func(key string) *outcome {
		if o, ok := decided[key]; ok {
			return o
		}
		o := &outcome{}
		decided[key] = o // a loop in malformed data finds this entry instead of recursing
		role := draft(key)
		o.role = role
		if role.SkippedReason != "" {
			return o
		}
		flat, err := bodyFor(key, map[string]bool{})
		if err != nil {
			o.role.SkippedReason = err.Error()
			o.role.FlattenedFrom = parentOf(key)
			return o
		}
		parent := parentOf(key)
		if parent != "" && parent != key {
			exists := false
			if _, ok := destination[parent]; ok {
				exists = true
			} else if _, inSource := source[parent]; inSource && !ledgered[parent] {
				exists = decide(parent).imported
			}
			if own := ownBody(key); exists && len(own) <= 32768 && ancestors(parent, 0)+1 <= maxRoleExtendsDepth {
				o.role.Extends, o.role.Body = parent, own
				o.imported = !ledgered[key]
				return o
			}
			o.role.FlattenedFrom = parent
		}
		if len(flat) > 32768 {
			o.role.SkippedReason = "flattened body exceeds v2 limit"
			return o
		}
		o.role.Body = flat
		o.imported = !ledgered[key]
		return o
	}
	for _, key := range keys {
		result = append(result, decide(key).role)
	}
	return result
}

func listGlobalRolesTx(ctx context.Context, tx *sql.Tx) (map[string]RoleDefinition, error) {
	roles, err := queryRoles(ctx, tx, ` WHERE project_id IS NULL AND deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	result := map[string]RoleDefinition{}
	for _, role := range roles {
		result[role.Key] = role
	}
	return result, nil
}

func importV1Roles(ctx context.Context, tx *sql.Tx, data *v1ImportData, mapping V1ImportMap, report *V1ImportReport) error {
	for key, reason := range data.SkippedSourceRoles {
		report.Skipped["roles"]++
		report.SkippedRoles[key] = reason
	}
	destination, err := listGlobalRolesTx(ctx, tx)
	if err != nil {
		return err
	}
	// A role an earlier run brought is not brought again, whether or not it is still there: it only counts as a parent if it is live.
	ledgered := map[string]bool{}
	for key := range data.Roles {
		_, found, err := importMapID(ctx, tx, mapping.SourceKey, "role", key)
		if err != nil {
			return err
		}
		ledgered[key] = found
	}
	resolved := resolveV1Roles(data.Roles, destination, ledgered)
	var linked []string // roles written with a parent, checked once every role is in place
	for _, role := range resolved {
		if role.SkippedReason != "" {
			report.Skipped["roles"]++
			report.SkippedRoles[role.Key] = role.SkippedReason
			continue
		}
		choice := mapping.RoleConflicts[role.Key]
		if choice == "" {
			choice = "keep_v2"
		}
		if choice != "keep_v2" && choice != "use_v1" {
			return fmt.Errorf("role %q has invalid conflict choice", role.Key)
		}
		_, found, err := importMapID(ctx, tx, mapping.SourceKey, "role", role.Key)
		if err != nil {
			return err
		}
		if found {
			report.AlreadyPresent["roles"]++
			continue
		}
		current, conflict := destination[role.Key]
		if conflict {
			report.RoleConflicts[role.Key] = choice
			if current.Body == role.Body && current.Extends == role.Extends && current.Emoji == role.Emoji && current.Cardinality == role.Cardinality && current.Label == role.Label {
				report.AlreadyPresent["roles"]++
			} else if choice == "keep_v2" {
				report.Skipped["role_conflicts"]++
			} else {
				if current.Cardinality != role.Cardinality {
					var live int
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE role_id=? AND revoked_at IS NULL AND (desired!='stopped' OR observed!='stopped')`, current.ID).Scan(&live); err != nil {
						return err
					}
					if live > 0 {
						return ErrRoleInUse
					}
				}
				body, fingerprint, base := importedRoleBody(current.ID, role.Body)
				if _, err := tx.ExecContext(ctx, `UPDATE role SET label=?,emoji=?,body=?,default_body_fingerprint=?,default_body_base=?,extends=?,cardinality=?,rev=rev+1 WHERE id=?`, role.Label, role.Emoji, body, fingerprint, base, role.Extends, role.Cardinality, current.ID); err != nil {
					return err
				}
				report.Imported["roles"]++
				if role.Extends != "" {
					linked = append(linked, role.Key)
				}
			}
		} else {
			id, err := newID("role")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO role(id,key,project_id,role_key,label,emoji,body,extends,cardinality) VALUES (?,?,NULL,?,?,?,?,?,?)`, id, role.Key, role.Key, role.Label, role.Emoji, role.Body, role.Extends, role.Cardinality); err != nil {
				return err
			}
			report.Imported["roles"]++
			if role.Extends != "" {
				linked = append(linked, role.Key)
			}
		}
		if err := addImportMap(ctx, tx, mapping.SourceKey, "role", role.Key, role.Key); err != nil {
			return err
		}
	}
	// A parent may be imported after its child (they go in key order), so the links are checked once all are in place. Resolution
	// already keeps them valid against what v2 has now; a choice that changes a parent (use_v1 over a v2 role with a different
	// chain) can still close a loop or deepen a chain. A link that would is flattened in place when the whole text fits, and the role
	// is otherwise left out as if it had been skipped (never stored with the parent's text missing); it never stops the import.
	for pass := 0; pass < 2*len(linked)+2 && len(linked) > 0; pass++ {
		all, err := allLiveRoles(ctx, tx)
		if err != nil {
			return err
		}
		repaired := false
		for _, key := range linked {
			for _, role := range all {
				if role.Key != key || role.Extends == "" || checkRoleExtends(role, all) == nil {
					continue
				}
				_, parentLive := roleParent(role, all)
				body := resolveRoleBody(role, all)
				if parentLive && len(body) <= 32768 {
					stored, fingerprint, base := importedRoleBody(role.ID, body)
					if _, err := tx.ExecContext(ctx, `UPDATE role SET body=?,default_body_fingerprint=?,default_body_base=?,extends='' WHERE id=?`, stored, fingerprint, base, role.ID); err != nil {
						return err
					}
					report.Skipped["role_links"]++
					report.FlattenedRoles = append(report.FlattenedRoles, key)
				} else if err := undoImportedRole(ctx, tx, mapping.SourceKey, role, destination, report); err != nil {
					return err
				}
				repaired = true
				break // `all` is stale now; the next pass looks again
			}
			if repaired {
				break
			}
		}
		if !repaired {
			break
		}
	}
	return nil
}

// undoImportedRole takes back a role this run wrote: a role it created is deleted, one it overwrote goes back to what v2 had, and its
// ledger entry is dropped, so the role reads as skipped (a later run may bring it).
func undoImportedRole(ctx context.Context, tx *sql.Tx, sourceKey string, role RoleDefinition, before map[string]RoleDefinition, report *V1ImportReport) error {
	if prior, ok := before[role.Key]; ok {
		stored := prior.Body
		if def, ok := builtinRoles()[prior.ID]; ok && stored == def.Body {
			stored = ""
		}
		if _, err := tx.ExecContext(ctx, `UPDATE role SET label=?,emoji=?,body=?,default_body_fingerprint=?,default_body_base=?,extends=?,cardinality=?,rev=rev+1 WHERE id=?`,
			prior.Label, prior.Emoji, stored, prior.DefaultBodyFingerprint, prior.DefaultBodyBase, prior.Extends, prior.Cardinality, role.ID); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM role WHERE id=?`, role.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM legacy_import_map WHERE source_key=? AND entity_kind='role' AND old_id=?`, sourceKey, role.Key); err != nil {
		return err
	}
	report.Imported["roles"]--
	report.Skipped["roles"]++
	report.SkippedRoles[role.Key] = "flattened body exceeds v2 limit"
	return nil
}
