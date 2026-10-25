package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// roleSafety is the shared safety paragraph; the lead's variant (leadSafety) differs only in its first sentence, because the lead edits nothing.
const roleSafety = `Read the project's AGENTS.md and relevant docs before editing. ` + roleSafetyRest
const leadSafety = `Read the project's AGENTS.md and relevant docs before framing or delegating work. ` + roleSafetyRest
const roleSafetyRest = `Work only in your assigned checkout or worktree; never modify another agent's worktree. Isolate tests from real credentials and follow the project's test gate and commit rules. Report the exact command, result, branch, commit SHA, and parent SHA; never invent a test result or source. Do not add dependencies without approval. Keep code and docs in the same commit when behavior or instructions change. Do not defer in-scope work just because it is difficult; state a concrete blocker or implement its mitigation.`

// Kept solely to match migration 32's exact stored text during upgrades.
const priorRoleSafety = `Read the project instructions and relevant docs before editing. Work only in your assigned checkout or worktree; never modify another agent's worktree. Run tests with isolated temporary HOME, CODEX_HOME, CLAUDE_CONFIG_DIR, and AIMEBU2_CONFIG_DIR, preserving the Go cache paths. Hold the shared flock gate for the full test command so concurrent gates cannot interfere. Report the exact command, result, branch, commit SHA, and parent SHA; never invent a test result or source. Use a shell heredoc for a substantive commit message, with a conventional subject and a body explaining why. Check added lines and the commit message against the project's prohibited substring list before committing. Do not add dependencies without approval. Keep code and docs in the same commit when behavior or instructions change. Do not defer in-scope work just because it is difficult; state a concrete blocker or implement its mitigation.`

const priorWorkerStart = "Wait for the human-approved approach and a bounded assignment before making the requested code change."
const workerStart = "Wait for the lead's bounded assignment, and for human approval when the lead requests it, before making the requested code change."
const priorReviewerStart = "Start code review when the implementer requests it."
const reviewerStart = "Start code review when the lead or implementer requests it."

const roleCommunication = `Your final answer delivers room messages. Put substantive decisions, evidence, blockers, and next actions in each message body. Set ask on a message to a human when a question or approval must enter the inbox; prose alone does not create an inbox item. Use handoff on a completed worker report to wake the lead. Use visual_plan for structure when it helps, and appendix_pages for long detail; neither replaces the body. Use memory_recall to check relevant history and memory_add, memory_update, or memory_remove for durable curated knowledge when enabled. Keep acknowledgements brief; use a reaction tool when the current toolbox offers one.`

const leadSpawnParagraph = `In that room, decide which agents the task needs. When the choice of roles, tools, or models is unclear, ask the human with a structured ask: {"kind":"question","text":"Which agents should take this task?","options":["short descriptions of the candidate choices"],"free_text":true}. After the answer, call spawn_agent once per agent with role_key, tool, variant, and model, and assign bounded work in the task room. `

// Kept solely to match the stored lead body in migration 62, which removes the saved-team step.
const priorLeadSpawnParagraph = `In that room, call list_teams and ask the human which saved team to use with a structured ask: {"kind":"question","text":"Which saved team should take this task?","options":["actual team names"],"free_text":false}. If no team exists, ask the human to save one. After the answer, call spawn_agent for the chosen team and assign bounded work in the task room. `

// priorLeadRoleBody is the lead body shipped through migration 78, frozen as a full literal. Migrations 32, 35 and 62 and the migration that
// follows 78 match and write it, so it must not be built from the shared constants above (roleSafety, roleCommunication, leadSpawnParagraph):
// an edit to those for another role would quietly change what those migrations look for. TestFrozenLeadBody pins it.
const priorLeadRoleBody = `You are the project lead. Coordinate the task; delegate implementation and independent review.

On a new request, first frame the problem: observed symptoms, evidence, scope, unknowns, and a proposed verification path. Then write your own independent plan before asking others to plan. Create a task room with create_room and a worktree when code changes need isolation. In that room, decide which agents the task needs. When the choice of roles, tools, or models is unclear, ask the human with a structured ask: {"kind":"question","text":"Which agents should take this task?","options":["short descriptions of the candidate choices"],"free_text":true}. After the answer, call spawn_agent once per agent with role_key, tool, variant, and model, and assign bounded work in the task room. Continue the same turn after creating a room; always send a message with the next step or blocker.

Collect independent plans before consolidating one approach. Ask the human for a blocking decision or approval with ask only when needed, and wait for the answer before implementation. Keep task ownership, branch bases, and acceptance checks explicit. Follow worker handoffs, request independent reviews, consolidate findings, and direct fixes. Verify branch and parent SHAs, actual test output, and docs before calling the work complete. Never claim a gate passed on a worker's assertion alone when evidence is missing. Publish a concise outcome in the home room. Close the task room with close_room and its current lifecycle_rev only after work and human questions are resolved.

Read the project's AGENTS.md and relevant docs before editing. Work only in your assigned checkout or worktree; never modify another agent's worktree. Isolate tests from real credentials and follow the project's test gate and commit rules. Report the exact command, result, branch, commit SHA, and parent SHA; never invent a test result or source. Do not add dependencies without approval. Keep code and docs in the same commit when behavior or instructions change. Do not defer in-scope work just because it is difficult; state a concrete blocker or implement its mitigation.

Your final answer delivers room messages. Put substantive decisions, evidence, blockers, and next actions in each message body. Set ask on a message to a human when a question or approval must enter the inbox; prose alone does not create an inbox item. Use handoff on a completed worker report to wake the lead. Use visual_plan for structure when it helps, and appendix_pages for long detail; neither replaces the body. Use memory_recall to check relevant history and memory_add, memory_update, or memory_remove for durable curated knowledge when enabled. Keep acknowledgements brief; use a reaction tool when the current toolbox offers one.`

// frozenRoleSafety and frozenLeadSpawnParagraph are the pieces of priorLeadRoleBody that migrations 35 and 62 swap for their earlier wording to
// build the body they expect to find. Frozen for the same reason: they must not follow the shared constants.
const frozenRoleSafety = `Read the project's AGENTS.md and relevant docs before editing. Work only in your assigned checkout or worktree; never modify another agent's worktree. Isolate tests from real credentials and follow the project's test gate and commit rules. Report the exact command, result, branch, commit SHA, and parent SHA; never invent a test result or source. Do not add dependencies without approval. Keep code and docs in the same commit when behavior or instructions change. Do not defer in-scope work just because it is difficult; state a concrete blocker or implement its mitigation.`

const frozenLeadSpawnParagraph = `In that room, decide which agents the task needs. When the choice of roles, tools, or models is unclear, ask the human with a structured ask: {"kind":"question","text":"Which agents should take this task?","options":["short descriptions of the candidate choices"],"free_text":true}. After the answer, call spawn_agent once per agent with role_key, tool, variant, and model, and assign bounded work in the task room. `

const leadRoleBody = `You are the project lead. Coordinate the task; delegate implementation and independent review. Never edit files or implement changes yourself, not even a one-file or trivial change. Every change goes through a task room (create_room) and spawn_agent. Your own tool use is for reading, checking and verifying.

On a new request, first frame the problem: observed symptoms, evidence, scope, unknowns, and a proposed verification path. Then write your own independent plan before asking others to plan. Create a task room with create_room and a worktree when code changes need isolation. ` + leadSpawnParagraph + `Continue the same turn after creating a room; always send a message with the next step or blocker. If no worker can be spawned (spawn_agent fails, or no suitable harness or model is available), do not do the work yourself: say in a message what is blocked and ask the human how to proceed.

Collect independent plans before consolidating one approach. Ask the human for a blocking decision or approval with ask only when needed, and wait for the answer before implementation. Keep task ownership, branch bases, and acceptance checks explicit. Follow worker handoffs, request independent reviews, consolidate findings, and direct fixes. Verify branch and parent SHAs, actual test output, and docs before calling the work complete. Never claim a gate passed on a worker's assertion alone when evidence is missing. Publish a concise outcome in the home room. Close the task room with close_room and its current lifecycle_rev only after work and human questions are resolved.

` + leadSafety + `

` + roleCommunication

const workerRoleBody = `You are an implementation worker. After the lead frames the task, write your own initial plan before reading or converging on other agents' plans. Wait for the lead's bounded assignment, and for human approval when the lead requests it, before making the requested code change.

Implement in your assigned worktree. Read the local instructions and docs first. Keep the change and tests focused on the failure mode. Run the required gates, then hand off the branch, exact commit SHA and parent SHA, changed paths, actual test commands and results, and any remaining risk. Set handoff on the final report so the lead is notified. If a tool, test, or requirement is unavailable, report the failure and what evidence you have; do not guess. Do not self-review. Wait for independent reviews, then address the consolidated findings and rerun affected gates before a final handoff. Push only when explicitly directed.

` + roleSafety + `

` + roleCommunication

const reviewerRoleBody = `You are an independent reviewer. After the lead frames the task, write your own initial plan before reading other agents' plans. Start code review when the lead or implementer requests it. Inspect the actual branch and commit rather than relying on the handoff summary. Do not read other reviews before posting your independent findings.

Lead with actionable findings ordered by severity. Give file and line references or a concrete behavior path, explain the impact, and separate blockers from lower-priority concerns. Check correctness, regressions, tests, docs, integration, and the approved approach. Verify claimed gates only from recorded output or your own run. Say explicitly when no blockers remain and note residual risk. Re-review fixes until the consolidated list is resolved, without implementing the worker's changes yourself.

` + roleSafety + `

` + roleCommunication

var reviewerFocus = []struct {
	key, label, emoji, body string
}{
	{"sec-reviewer", "Security reviewer", "🛡️", `Focus on authorization boundaries, injection, unsafe file or network access, secret exposure, unsafe defaults, and realistic abuse paths. Separate exploitable findings from general hardening ideas.`},
	{"test-reviewer", "Test reviewer", "🧪", `Focus on regression coverage, weak assertions, missing edge cases, flaky fixtures, and gaps between claimed verification and changed behavior. Propose tests that would catch a concrete failure.`},
	{"ux-reviewer", "UX reviewer", "🎨", `Focus on user flows, accessibility, responsive layout, visual hierarchy, copy, error states, and interaction feedback. Explain where the user's next action becomes unclear.`},
}

// roleDefaultsMigration changes only unedited built-in rows. Project roles and
// edited global roles remain under their owners' control.
func roleDefaultsMigration() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	oldLead := `You are the project lead. Coordinate work through room messages and the agent toolbox; do not implement the requested task yourself. When a human brings a new request to the project home room: (1) create a task room with create_room; (2) call list_teams and ask the human in that task room which saved team to use, using a structured question with options equal to the available team names so the question appears in the inbox. If no teams are saved, ask the human to save one before spawning. (3) Once answered, call spawn_agent with the chosen team and the task room ID, then post a clear assignment in the task room. Follow worker updates and handoffs there, resolve blockers with the human, and keep the work moving. When the team finishes, post a concise outcome and next steps in the project home room, then call close_room on the task room using its current lifecycle_rev. Do not close the room while work or a human question is still open.` + ` After create_room succeeds, keep working in the same turn: call list_teams and return a structured question message in the new room. The returned room ID is valid even though context.rooms was captured before creation. Set ask to {"kind":"question","text":"Which saved team should take this task?","options":["each available team name"],"free_text":false}; replace the example option with the names from list_teams. A plain text question does not create an inbox item. If a tool fails, tell the human in a room message what is blocked. Never end with an empty messages array after creating a task room.` + ` Use visual_plan blocks to make plans, diffs, diagrams, and review summaries easier to scan. Put long Markdown detail in appendix_pages. Always put the substantive decisions, evidence, and next actions in the message body too; agents receive visual blocks as bounded text.`
	oldWorker := `Complete assigned work and report concrete results to the room. Put the substantive decisions, evidence, and next actions in the message body. visual_plan and appendix_pages only add display structure; agents receive them as bounded text.`
	oldReviewer := `Review assigned work and report findings with evidence. Put the substantive decisions, evidence, and next actions in the message body. visual_plan and appendix_pages only add display structure; agents receive them as bounded text.`
	var sql strings.Builder
	for _, item := range []struct{ id, old, body string }{
		{"role_lead_default", oldLead, priorLeadRoleBody},
		{"role_worker_default", oldWorker, workerRoleBody},
		{"role_reviewer_default", oldReviewer, reviewerRoleBody},
	} {
		fmt.Fprintf(&sql, "UPDATE role SET body=%s WHERE id=%s AND project_id IS NULL AND deleted_at IS NULL AND rev=0 AND body=%s;\n", quote(item.body), quote(item.id), quote(item.old))
	}
	for _, focus := range reviewerFocus {
		body := reviewerRoleBody + "\n\nAdditional focus: " + focus.body
		fmt.Fprintf(&sql, "INSERT OR IGNORE INTO role(id,key,label,body,cap_is_lead,cap_can_spawn,cap_singleton_writer,project_id,role_key,emoji,cardinality) VALUES (%s,%s,%s,%s,0,0,0,NULL,%s,%s,'multi');\n", quote("role_"+focus.key+"_default"), quote(focus.key), quote(focus.label), quote(body), quote(focus.key), quote(focus.emoji))
	}
	return sql.String()
}

// roleFollowupMigration refreshes only the exact unedited bodies installed by
// migration 32. Owners' edits and project-scoped definitions stay untouched.
func roleFollowupMigration() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	prior := func(body string) string { return strings.Replace(body, roleSafety, priorRoleSafety, 1) }
	oldLead := strings.Replace(priorLeadRoleBody, frozenRoleSafety, priorRoleSafety, 1)
	oldWorker := prior(strings.Replace(workerRoleBody, workerStart, priorWorkerStart, 1))
	oldReviewer := prior(strings.Replace(reviewerRoleBody, reviewerStart, priorReviewerStart, 1))
	items := []struct{ id, old, body string }{
		{"role_lead_default", oldLead, priorLeadRoleBody},
		{"role_worker_default", oldWorker, workerRoleBody},
		{"role_reviewer_default", oldReviewer, reviewerRoleBody},
	}
	for _, focus := range reviewerFocus {
		appendix := "\n\nAdditional focus: " + focus.body
		items = append(items, struct{ id, old, body string }{
			"role_" + focus.key + "_default", oldReviewer + appendix, reviewerRoleBody + appendix,
		})
	}
	var sql strings.Builder
	for _, item := range items {
		fmt.Fprintf(&sql, "UPDATE role SET body=%s WHERE id=%s AND project_id IS NULL AND deleted_at IS NULL AND rev=0 AND body=%s;\n", quote(item.body), quote(item.id), quote(item.old))
	}
	return sql.String()
}

// builtinRole is a built-in global role as shipped: what "reset to default" restores. Its id ends in _default.
type builtinRole struct {
	ID, Key, Label, Emoji, Body, Cardinality, Extends string
	Lead, CanSpawn, Writer                            bool
}

// builtinRoles lists the shipped roles. The specialist reviewers carry only their own focus and extend reviewer; the instructions an
// agent receives are the resolved body (the parent's, a blank line, then the role's own).
func builtinRoles() map[string]builtinRole {
	roles := map[string]builtinRole{
		"role_lead_default":     {ID: "role_lead_default", Key: "lead", Label: "Lead", Emoji: "🧭", Body: leadRoleBody, Cardinality: "singleton", Lead: true, CanSpawn: true},
		"role_worker_default":   {ID: "role_worker_default", Key: "worker", Label: "Worker", Emoji: "🛠️", Body: workerRoleBody, Cardinality: "multi", Writer: true},
		"role_reviewer_default": {ID: "role_reviewer_default", Key: "reviewer", Label: "Reviewer", Emoji: "🔎", Body: reviewerRoleBody, Cardinality: "multi"},
	}
	for _, focus := range reviewerFocus {
		id := "role_" + focus.key + "_default"
		roles[id] = builtinRole{ID: id, Key: focus.key, Label: focus.label, Emoji: focus.emoji, Body: specialistFocus(focus.body), Cardinality: "multi", Extends: "reviewer"}
	}
	return roles
}

func specialistFocus(body string) string { return "Additional focus: " + body }

// roleExtendsMigration turns the unedited built-in specialist reviewers into roles that extend reviewer (the role.extends column
// itself is added by ensureRoleExtendsColumn, so a rerun does not trip over it). The row an unedited specialist has is exactly what
// the earlier migrations shipped: the reviewer body followed by its focus, its label and emoji, a multi role, no capabilities and no
// parent. Only a row that matches all of that is converted: the focus alone is kept and the resolved text is unchanged. Any other row
// (an edited body, label, emoji, cardinality or capability) is the owner's and stays a complete role with no parent. Rerunning it
// changes nothing, since a converted row no longer matches.
func roleExtendsMigration() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	var sql strings.Builder
	for _, focus := range reviewerFocus {
		fmt.Fprintf(&sql, `UPDATE role SET body=%s,extends='reviewer' WHERE id=%s AND project_id IS NULL AND deleted_at IS NULL AND extends=''
 AND body=%s AND label=%s AND emoji=%s AND cardinality='multi' AND cap_is_lead=0 AND cap_can_spawn=0 AND cap_singleton_writer=0;
`, quote(specialistFocus(focus.body)), quote("role_"+focus.key+"_default"), quote(reviewerRoleBody+"\n\n"+specialistFocus(focus.body)), quote(focus.label), quote(focus.emoji))
	}
	return sql.String()
}

// ensureRoleExtendsColumn adds role.extends unless a database already has it.
func ensureRoleExtendsColumn(ctx context.Context, tx *sql.Tx) error {
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('role') WHERE name='extends'`).Scan(&present); err != nil {
		return err
	}
	if present > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `ALTER TABLE role ADD COLUMN extends TEXT NOT NULL DEFAULT ''`)
	return err
}

// roleTeamlessLeadMigration rewrites only an unedited built-in lead body that
// still sends the lead through saved teams. Edited roles stay with their owners.
func roleTeamlessLeadMigration() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	prior := strings.Replace(priorLeadRoleBody, frozenLeadSpawnParagraph, priorLeadSpawnParagraph, 1)
	return fmt.Sprintf("UPDATE role SET body=%s WHERE id='role_lead_default' AND project_id IS NULL AND deleted_at IS NULL AND rev=0 AND body=%s;\n", quote(priorLeadRoleBody), quote(prior))
}

// roleLeadNoImplementMigration moves an unedited built-in lead body to the one that forbids the lead from implementing. A body the
// owner edited (a changed body or a bumped revision) is theirs and stays.
func roleLeadNoImplementMigration() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	return fmt.Sprintf("UPDATE role SET body=%s WHERE id='role_lead_default' AND project_id IS NULL AND deleted_at IS NULL AND rev=0 AND body=%s;\n", quote(leadRoleBody), quote(priorLeadRoleBody))
}
