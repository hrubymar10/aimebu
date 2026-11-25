package store

import (
	"fmt"
	"strings"
)

// roleSpawnApprovalMigration (migration 134) moves the unedited built-in
// leader and dispatcher bodies to the ones carrying the model-approval rule. A
// body the owner edited (a changed body or a bumped revision) stays until
// migration 139 archives it and switches the role to the running default.
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
// its own policy until migration 139 archives it.
func roleResearcherMigration() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	return fmt.Sprintf("UPDATE role SET body=%s WHERE id='role_dispatcher_default' AND project_id IS NULL AND deleted_at IS NULL AND rev=0 AND body=%s;\n",
		quote(finalDispatcherRoleBody), quote(currentDispatcherRoleBody))
}
