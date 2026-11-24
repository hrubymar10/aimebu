package store

import (
	"context"
	"fmt"
	"strings"
)

// roleSpawnApprovalMigration (migration 134) moves the unedited built-in
// leader and dispatcher bodies to the ones carrying the model-approval rule. A
// body the owner edited (a changed body or a bumped revision) is theirs and
// stays; spawnApprovalRoleFacts reports it once so the owner knows the
// sentence is missing.
func roleSpawnApprovalMigration() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	var sql strings.Builder
	fmt.Fprintf(&sql, "UPDATE role SET body=%s WHERE id='role_leader_default' AND project_id IS NULL AND deleted_at IS NULL AND rev=1 AND body=%s;\n",
		quote(currentLeadRoleBody), quote(leadRoleBody))
	fmt.Fprintf(&sql, "UPDATE role SET body=%s WHERE id='role_dispatcher_default' AND project_id IS NULL AND deleted_at IS NULL AND rev=0 AND body=%s;\n",
		quote(currentDispatcherRoleBody), quote(dispatcherRoleBody))
	return sql.String()
}

// roleResearcherMigration (migration 135) moves the unedited built-in
// dispatcher body on from migration 134's text to the one that also carries
// the who-spawns-what rule. The leader body needs no second rewrite: the
// researcher sentence about the dispatcher belongs only to the
// leader-dispatcher flow's leader text, which ships it in code, not to the
// base leader body a dispatcher-less flow delivers. An owner-edited body keeps
// its own policy; spawnApprovalRoleFacts reports it once.
func roleResearcherMigration() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	return fmt.Sprintf("UPDATE role SET body=%s WHERE id='role_dispatcher_default' AND project_id IS NULL AND deleted_at IS NULL AND rev=0 AND body=%s;\n",
		quote(finalDispatcherRoleBody), quote(currentDispatcherRoleBody))
}

// spawnApprovalRoleFacts runs in migration 135, after 134 has rewritten the
// unedited defaults, and names each built-in leader or dispatcher body that
// neither migration rewrites and that is missing its shipped sentence (the
// model-approval rule for both, the researcher rule for the dispatcher): the
// owner's edit kept its own policy, and this is its one notice.
func spawnApprovalRoleFacts(ctx context.Context, q rowQuerier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,body FROM role
		WHERE id IN ('role_leader_default','role_dispatcher_default')
		AND project_id IS NULL AND deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []string
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		if body == currentDispatcherRoleBody {
			continue // migration 135 rewrites this unedited default
		}
		if id == "role_leader_default" {
			if body == currentLeadRoleBody || strings.Contains(body, spawnApprovalRuleLead) {
				continue // the owner already carries the rule
			}
		} else if strings.Contains(body, spawnApprovalRuleDispatcher) && strings.Contains(body, researcherRuleDispatcher) {
			continue // the owner already carries both rules
		}
		missing := ""
		if id == "role_dispatcher_default" && strings.Contains(body, spawnApprovalRuleDispatcher) {
			missing = " or the researcher rule"
		}
		facts = append(facts, fmt.Sprintf("built-in role %s was edited by its owner and is missing a shipped sentence (the model-approval rule%s); reset it to the default or add the sentences by hand", id, missing))
	}
	return facts, rows.Err()
}
