package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func extensionFixture(t *testing.T) (*Store, Project) {
	t.Helper()
	s, project, _, _, _ := promptFixture(t)
	return s, project
}

func TestSetRoleBodyExtensionPermissions(t *testing.T) {
	t.Parallel()
	s, project := extensionFixture(t)
	ctx := context.Background()

	// A second project with its own operator, who belongs to no other project.
	other, err := s.CreateProject(ctx, Project{Name: "other", Folder: "/tmp/other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_other','other','hash','{}','4')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, other.ID, "usr_other", "operator"); err != nil {
		t.Fatal(err)
	}
	// An admin: trusted like the owner for global roles, but the extension is the owner's alone.
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,admin,created_at) VALUES ('usr_admin','admin','hash','{}',1,'5')`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", "", "role_worker", "global rules", 0); err != nil {
		t.Fatalf("the owner sets the global extension: %v", err)
	}
	for _, test := range []struct{ name, actor, projectID string }{
		{"an admin may not set the global extension", "usr_admin", ""},
		{"a project operator may not set the global extension", "usr_operator", ""},
		{"a project viewer may not set a project extension", "usr_viewer", project.ID},
	} {
		if _, err := s.SetRoleBodyExtension(ctx, test.actor, test.projectID, "role_worker", "rules", 0); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: err=%v, want ErrForbidden", test.name, err)
		}
	}
	// A person of another project (its operator here) is no member of this one and is refused.
	if _, err := s.SetRoleBodyExtension(ctx, "usr_other", project.ID, "role_worker", "rules", 0); err == nil || (errors.Is(err, ErrForbidden) == false && errors.Is(err, sql.ErrNoRows) == false) {
		t.Errorf("another project's operator: err=%v, want refused", err)
	}
	if _, err := s.SetRoleBodyExtension(ctx, "usr_operator", project.ID, "role_worker", "project rules", 0); err != nil {
		t.Fatalf("a project operator sets the project extension: %v", err)
	}
	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", project.ID, "role_worker", "owner project rules", 1); err != nil {
		t.Fatalf("the project owner sets the project extension: %v", err)
	}

	// Revisions: change with the current one, a stale one is refused.
	if _, err := s.SetRoleBodyExtension(ctx, "usr_operator", project.ID, "role_worker", "project rules 2", 7); !errors.Is(err, ErrStaleRev) {
		t.Errorf("a stale revision: err=%v, want ErrStaleRev", err)
	}
	saved, err := s.SetRoleBodyExtension(ctx, "usr_operator", project.ID, "role_worker", "project rules 2", 2)
	if err != nil || saved.Rev != 3 || saved.Body != "project rules 2" {
		t.Fatalf("change: %+v err=%v", saved, err)
	}

	// The size limit, with a clear error.
	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", "", "role_worker", strings.Repeat("x", MaxRoleBodyExtensionLen+1), 1); !errors.Is(err, ErrRoleBodyExtension) {
		t.Errorf("an oversized extension: err=%v, want ErrRoleBodyExtension", err)
	}
	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", "", "role_worker", strings.Repeat("x", MaxRoleBodyExtensionLen), 1); err != nil {
		t.Fatalf("an extension at the limit: %v", err)
	}

	// Text is trimmed; an empty or blank body clears the row.
	saved, err = s.SetRoleBodyExtension(ctx, "usr_owner", "", "role_worker", "  trimmed rules  ", 2)
	if err != nil || saved.Body != "trimmed rules" || saved.Rev != 3 {
		t.Fatalf("trim: %+v err=%v", saved, err)
	}
	saved, err = s.SetRoleBodyExtension(ctx, "usr_owner", "", "role_worker", "", 3)
	if err != nil || saved.Body != "" || saved.Rev != 0 {
		t.Fatalf("clear: %+v err=%v", saved, err)
	}
	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", "", "role_worker", "", 4); !errors.Is(err, ErrStaleRev) {
		t.Errorf("clearing an absent extension with a revision: err=%v, want ErrStaleRev", err)
	}

	// An unknown or deleted role takes no extension.
	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", "", "role_gone", "rules", 0); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("an unknown role: err=%v, want sql.ErrNoRows", err)
	}
}

func TestDeliveredRoleBodyComposition(t *testing.T) {
	t.Parallel()
	s, project := extensionFixture(t)
	ctx := context.Background()

	roleID := "role_worker"
	base := func() string {
		var out string
		if err := s.Tx(ctx, func(tx *Tx) error {
			var err error
			out, _, err = tx.deliveredRoleBody(roleID, project.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}()
	if !strings.Contains(base, "work") {
		t.Fatalf("base=%q", base)
	}

	set := func(projectID, body string) {
		t.Helper()
		if _, err := s.db.Exec(`DELETE FROM role_body_extension WHERE role_id=? AND COALESCE(project_id,'')=?`, roleID, projectID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", projectID, roleID, body, 0); err != nil {
			t.Fatalf("set extension: %v", err)
		}
	}
	delivered := func() string {
		t.Helper()
		var out string
		if err := s.Tx(ctx, func(tx *Tx) error {
			var err error
			out, _, err = tx.deliveredRoleBody(roleID, project.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// No extension: the base stands alone.
	if got := delivered(); got != base {
		t.Fatalf("no extension: %q, want %q", got, base)
	}
	// Global only, project only, both: each appends after the base, in that order, blank-line joined.
	set("", "global rules")
	if got, want := delivered(), base+"\n\nglobal rules"; got != want {
		t.Fatalf("global only: %q, want %q", got, want)
	}
	set(project.ID, "project rules")
	if got, want := delivered(), base+"\n\nglobal rules\n\nproject rules"; got != want {
		t.Fatalf("both: %q, want %q", got, want)
	}
	set("", "")
	if got, want := delivered(), base+"\n\nproject rules"; got != want {
		t.Fatalf("project only: %q, want %q", got, want)
	}
	// An empty part is skipped, not left as a stray blank line: a blank row saved by hand changes nothing.
	if _, err := s.db.Exec(`INSERT INTO role_body_extension(role_id,project_id,body,rev,updated_at) VALUES (?,?,?,1,'now')`, roleID, nil, "   "); err != nil {
		t.Fatal(err)
	}
	if got := delivered(); got != base+"\n\nproject rules" {
		t.Fatalf("blank global part: %q", got)
	}

	// An edited body is the base the extensions append to, and the row's own text is unchanged.
	definition, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range definition {
		if role.ID == roleID {
			edited := role
			edited.Body = "edited work"
			if _, err := s.ManageRole(ctx, "usr_owner", edited, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	set("", "global rules")
	if got, want := delivered(), "edited work\n\nglobal rules\n\nproject rules"; got != want {
		t.Fatalf("edited base: %q, want %q", got, want)
	}

	// A flow's role-text override replaces the base; the extensions still append. The dispatcher of the leader-dispatcher
	// flow carries an override, extensions, and the duplicate-spawn instruction.
	if _, err := s.db.Exec(`UPDATE project SET team_flow_key='leader-dispatcher' WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	flow, ok := teamFlowTemplate("leader-dispatcher")
	if !ok {
		t.Fatal("no leader-dispatcher template")
	}
	dispatcherID := "role_dispatcher_default"
	if _, err := s.db.Exec(`DELETE FROM role_body_extension WHERE role_id=? AND COALESCE(project_id,'') IN ('',?)`, dispatcherID, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", "", dispatcherID, "global rules", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", project.ID, dispatcherID, "project rules", 0); err != nil {
		t.Fatal(err)
	}
	override := flow.RoleTextOverrides["dispatcher"]
	if override == "" {
		t.Fatal("the flow has no dispatcher override")
	}
	var dispatcherDelivered string
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		dispatcherDelivered, _, err = tx.deliveredRoleBody(dispatcherID, project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := override + "\n\nglobal rules\n\nproject rules"; !strings.HasPrefix(dispatcherDelivered, want) || !strings.Contains(dispatcherDelivered, "409 idle_agent_exists") {
		t.Fatalf("flow override: %q, want %q", dispatcherDelivered, want)
	}
}

func TestRoleBodyExtensionMigrationKeepsTheDefaultRewrite(t *testing.T) {
	t.Parallel()
	db := databaseAtVersion(t, filepath.Join(t.TempDir(), "extension.sqlite"), LatestSchemaVersion())
	defer db.Close()
	// An unedited built-in body with an extension: the migration still rewrites the body, and the extension survives.
	if _, err := db.Exec(`UPDATE role SET body=?,rev=0 WHERE id='role_worker_default'`, workerRoleBody); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO role_body_extension(role_id,project_id,body,rev,updated_at) VALUES ('role_worker_default',NULL,'project rules',1,'now')`); err != nil {
		t.Fatal(err)
	}
	// An edited base keeps its own text.
	if _, err := db.Exec(`UPDATE role SET body=?,rev=0 WHERE id='role_reviewer_default'`, reviewerRoleBody+" owner edit"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(roleRoomMentionMigration()); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := db.QueryRow(`SELECT body FROM role WHERE id='role_worker_default'`).Scan(&body); err != nil || body != currentWorkerRoleBody {
		t.Fatalf("unedited with extension: body=%q err=%v", body, err)
	}
	if err := db.QueryRow(`SELECT body FROM role WHERE id='role_reviewer_default'`).Scan(&body); err != nil || body != reviewerRoleBody+" owner edit" {
		t.Fatalf("edited base: body=%q err=%v", body, err)
	}
	var extension string
	if err := db.QueryRow(`SELECT body FROM role_body_extension WHERE role_id='role_worker_default'`).Scan(&extension); err != nil || extension != "project rules" {
		t.Fatalf("extension=%q err=%v", extension, err)
	}
}

func TestManageRoleCarriesTheExtension(t *testing.T) {
	t.Parallel()
	s, _ := extensionFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,admin,created_at) VALUES ('usr_admin','admin','hash','{}',1,'5')`); err != nil {
		t.Fatal(err)
	}

	// The owner saves an extension with the role write; an admin's write that omits the field keeps it.
	extension := "instance rules"
	definition, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var worker RoleDefinition
	for _, role := range definition {
		if role.ID == "role_worker" {
			worker = role
		}
	}
	worker.BodyExtension = &extension
	if _, err := s.ManageRole(ctx, "usr_owner", worker, false); err != nil {
		t.Fatalf("the owner saves the extension with the role: %v", err)
	}
	listed, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range listed {
		if role.ID == "role_worker" && (role.BodyExtension == nil || *role.BodyExtension != "instance rules") {
			t.Fatalf("listed extension=%v", role.BodyExtension)
		}
	}
	admin := worker
	admin.BodyExtension = nil
	admin.Body = "work, edited"
	fresh, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range fresh {
		if role.ID == "role_worker" {
			admin.Rev = role.Rev
		}
	}
	if _, err := s.ManageRole(ctx, "usr_admin", admin, false); err != nil {
		t.Fatalf("an admin edits the role: %v", err)
	}
	listed, err = s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range listed {
		if role.ID == "role_worker" {
			if role.Body != "work, edited" {
				t.Fatalf("body=%q", role.Body)
			}
			if role.BodyExtension == nil || *role.BodyExtension != "instance rules" {
				t.Fatalf("an omitted field must keep the extension, got %v", role.BodyExtension)
			}
		}
	}
	// An admin sending the field is refused: it is the owner's.
	adminBody := "admin rules"
	admin.BodyExtension = &adminBody
	fresh, err = s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range fresh {
		if role.ID == "role_worker" {
			admin.Rev = role.Rev
		}
	}
	if _, err := s.ManageRole(ctx, "usr_admin", admin, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an admin sends the extension: err=%v, want ErrForbidden", err)
	}
	// The owner clears it with an empty field.
	empty := ""
	worker.BodyExtension = &empty
	fresh, err = s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range fresh {
		if role.ID == "role_worker" {
			worker.Rev = role.Rev
		}
	}
	if _, err := s.ManageRole(ctx, "usr_owner", worker, false); err != nil {
		t.Fatalf("the owner clears the extension: %v", err)
	}
	listed, err = s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range listed {
		if role.ID == "role_worker" && role.BodyExtension != nil {
			t.Fatalf("extension survives a clear: %v", *role.BodyExtension)
		}
	}
}

func TestRoleDeletionRemovesItsExtensions(t *testing.T) {
	t.Parallel()
	s, _ := extensionFixture(t)
	ctx := context.Background()
	created, err := s.ManageRole(ctx, "usr_owner", RoleDefinition{Key: "tempspec", Label: "Temp spec", Body: "temporary role", Cardinality: "multi"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", "", created.ID, "rules", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", RoleDefinition{ID: created.ID, Rev: created.Rev}, true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM role_body_extension WHERE role_id=?`, created.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("extensions after deletion: count=%d err=%v", count, err)
	}
}

func TestRolePromptSettingsView(t *testing.T) {
	t.Parallel()
	s, project := extensionFixture(t)
	ctx := context.Background()
	if _, err := s.SetRoleBodyExtension(ctx, "usr_owner", "", "role_worker", "global rules", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRoleBodyExtension(ctx, "usr_operator", project.ID, "role_worker", "project rules", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE project SET team_flow_key='leader-dispatcher' WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.RolePromptSettings(ctx, "usr_operator", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var worker *RolePromptSettings
	for i := range rows {
		if rows[i].RoleKey == "worker" {
			worker = &rows[i]
		}
	}
	if worker == nil {
		t.Fatal("no worker row")
	}
	if worker.GlobalBodyExtension != "global rules" || worker.Project == nil || worker.Project.Body != "project rules" || worker.Project.Rev != 1 {
		t.Fatalf("extensions: %+v", worker)
	}
	// The dispatcher's row shows the flow override; the worker's delivered text carries the flow's task-room reporting sentence.
	for _, row := range rows {
		if row.RoleKey == "dispatcher" && !row.FlowOverride {
			t.Error("the dispatcher row misses the flow override flag")
		}
	}
	if !strings.Contains(worker.DeliveredBody, "global rules") || !strings.Contains(worker.DeliveredBody, "project rules") {
		t.Fatalf("delivered=%q", worker.DeliveredBody)
	}
	if !strings.HasSuffix(worker.DeliveredBody, "the leader is not a member of task rooms in this flow.") {
		t.Errorf("the delivered text misses the flow's worker addition: %q", worker.DeliveredBody)
	}
	// A viewer is refused.
	if _, err := s.RolePromptSettings(ctx, "usr_viewer", project.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a viewer reads the settings: err=%v, want ErrForbidden", err)
	}
}
