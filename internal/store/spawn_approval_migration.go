package store

import (
	"context"
	"fmt"
	"strings"
)

// roleSpawnApprovalMigration moves the unedited built-in leader and dispatcher
// bodies to the ones carrying the model-approval rule. A body the owner edited
// (a changed body or a bumped revision) is theirs and stays; spawnApprovalRoleFacts
// reports it once so the owner knows the sentence is missing.
func roleSpawnApprovalMigration() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	var sql strings.Builder
	fmt.Fprintf(&sql, "UPDATE role SET body=%s WHERE id='role_leader_default' AND project_id IS NULL AND deleted_at IS NULL AND rev=1 AND body=%s;\n",
		quote(currentLeadRoleBody), quote(leadRoleBody))
	fmt.Fprintf(&sql, "UPDATE role SET body=%s WHERE id='role_dispatcher_default' AND project_id IS NULL AND deleted_at IS NULL AND rev=0 AND body=%s;\n",
		quote(currentDispatcherRoleBody), quote(dispatcherRoleBody))
	return sql.String()
}

// spawnApprovalRoleFacts runs before the migration's SQL and names each
// built-in leader or dispatcher body that does not carry the approval rule and
// is not one the migration rewrites: the owner's edit kept its own policy, and
// this is its one notice that the sentence is missing.
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
		if body == leadRoleBody || body == dispatcherRoleBody {
			continue // the migration rewrites this unedited default
		}
		if strings.Contains(body, spawnApprovalRuleLead) || strings.Contains(body, spawnApprovalRuleDispatcher) {
			continue // the owner already carries the rule
		}
		facts = append(facts, fmt.Sprintf("built-in role %s was edited by its owner and is missing the model-approval sentence; reset it to the default or add the sentence by hand", id))
	}
	return facts, rows.Err()
}
