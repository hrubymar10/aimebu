package server

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func decodeBackupBody(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

// seedBackupSettings gives a fixture some of every kind of setting a backup carries.
func seedBackupSettings(t *testing.T, f *fixture) {
	t.Helper()
	for _, step := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"PUT", "/api/macros", map[string]string{"greet": "Hello <name>", "sign": "Thanks"}, 200},
		{"PUT", "/api/retention", map[string]int{"messages_considered_expired_after_seconds": 7200}, 200},
		{"POST", "/api/roles", map[string]any{"role_key": "scribe", "label": "Scribe", "emoji": "📝", "body": "Write things down.", "cardinality": "multi"}, 201},
	} {
		resp := f.request(step.method, step.path, step.body, true)
		resp.Body.Close()
		if resp.StatusCode != step.want {
			t.Fatalf("%s %s: %d", step.method, step.path, resp.StatusCode)
		}
	}
}

func exportBackup(t *testing.T, f *fixture) map[string]any {
	t.Helper()
	resp := f.request("GET", "/api/settings-backup", nil, false)
	if resp.StatusCode != 200 {
		t.Fatalf("export: %d", resp.StatusCode)
	}
	var backup map[string]any
	decodeBackupBody(t, resp, &backup)
	return backup
}

func previewBackup(t *testing.T, f *fixture, backup map[string]any) (int, backupPreview) {
	t.Helper()
	resp := f.request("POST", "/api/settings-backup/preview", map[string]any{"backup": backup}, true)
	var preview backupPreview
	status := resp.StatusCode
	decodeBackupBody(t, resp, &preview)
	return status, preview
}

func sectionOf(preview backupPreview, name string) backupSectionPreview {
	for _, section := range preview.Sections {
		if section.Section == name {
			return section
		}
	}
	return backupSectionPreview{Section: name}
}

func TestSettingsBackupExportCarriesOnlySettingsAndNoSecrets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	seedBackupSettings(t, f)
	backup := exportBackup(t, f)
	if backup["schema"] != settingsBackupSchema || backup["version"] != float64(settingsBackupVersion) {
		t.Fatalf("identity: %v %v", backup["schema"], backup["version"])
	}
	allowed := map[string]bool{"schema": true, "version": true, "exported_at": true, "macros": true, "roles": true,
		"retention": true, "quota_switcher": true, "limit_guard": true, "ui_prefs": true}
	for key := range backup {
		if !allowed[key] {
			t.Fatalf("unexpected top-level section %q", key)
		}
	}
	// No field anywhere in the file is shaped like a credential.
	secretish := regexp.MustCompile(`(?i)token|secret|password|credential|api[_-]?key|cookie|bearer|session|auth`)
	var walk func(path string, value any)
	walk = func(path string, value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, inner := range v {
				if secretish.MatchString(key) && path != "macros" {
					t.Fatalf("field %s.%s looks like a secret", path, key)
				}
				walk(path+"."+key, inner)
			}
		case []any:
			for _, inner := range v {
				walk(path, inner)
			}
		}
	}
	walk("", backup)
	roles, _ := backup["roles"].([]any)
	var keys []string
	for _, role := range roles {
		keys = append(keys, role.(map[string]any)["role_key"].(string))
	}
	joined := strings.Join(keys, ",")
	if !strings.Contains(joined, "scribe") {
		t.Fatalf("the exported roles: %v", keys)
	}
	if backup["macros"].(map[string]any)["greet"] != "Hello <name>" || backup["retention"].(map[string]any)["messages_considered_expired_after_seconds"] != float64(7200) {
		t.Fatalf("settings missing: %v", backup)
	}
}

func TestSettingsBackupRoundTripIntoFreshInstance(t *testing.T) {
	t.Parallel()
	source := newFixture(t)
	source.seed()
	seedBackupSettings(t, source)
	backup := exportBackup(t, source)

	target := newFixture(t)
	status, preview := previewBackup(t, target, backup)
	if status != 200 || preview.Digest == "" {
		t.Fatalf("preview: %d %+v", status, preview)
	}
	if got := sectionOf(preview, "macros"); got.New != 2 || got.Overwrite != 0 {
		t.Fatalf("macros preview: %+v", got)
	}
	// Previewing saved nothing.
	var macros map[string]string
	resp := target.request("GET", "/api/macros", nil, false)
	decodeBackupBody(t, resp, &macros)
	if macros["greet"] != "" {
		t.Fatalf("preview saved macros: %v", macros)
	}
	resp = target.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": preview.Digest}, true)
	var result backupResult
	if resp.StatusCode != 200 {
		t.Fatalf("apply: %d", resp.StatusCode)
	}
	decodeBackupBody(t, resp, &result)
	if !result.Complete || len(result.Failed) != 0 {
		t.Fatalf("apply result: %+v", result)
	}
	again := exportBackup(t, target)
	// A fresh instance ships default macros; importing merges into them, so every imported macro is there and the defaults stay.
	for key, body := range backup["macros"].(map[string]any) {
		if again["macros"].(map[string]any)[key] != body {
			t.Fatalf("macro %q after import: %v", key, again["macros"].(map[string]any)[key])
		}
	}
	for _, section := range []string{"retention"} {
		want, _ := json.Marshal(backup[section])
		got, _ := json.Marshal(again[section])
		if string(want) != string(got) {
			t.Fatalf("%s differs after import:\nwant %s\n got %s", section, want, got)
		}
	}
	roleKeys := func(b map[string]any) string {
		var keys []string
		for _, role := range b["roles"].([]any) {
			keys = append(keys, role.(map[string]any)["role_key"].(string)+":"+role.(map[string]any)["body"].(string))
		}
		return strings.Join(keys, "|")
	}
	if roleKeys(backup) != roleKeys(again) {
		t.Fatalf("roles differ:\n%s\n%s", roleKeys(backup), roleKeys(again))
	}
	// Previewing the same file again finds nothing left to do.
	_, second := previewBackup(t, target, backup)
	for _, section := range second.Sections {
		if section.New != 0 || section.Overwrite != 0 {
			t.Fatalf("a second preview still has work: %+v", section)
		}
	}
}

func TestSettingsBackupIsOwnerOnlyAndChecksIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	backup := exportBackup(t, f)
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/settings-backup", nil},
		{"POST", "/api/settings-backup/preview", map[string]any{"backup": backup}},
		{"POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": "x"}},
	} {
		resp := f.requestAs(t, cookie, csrf, call.method, call.path, call.body)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("non-owner %s %s: %d", call.method, call.path, resp.StatusCode)
		}
	}
	resp := f.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": "x"}, false)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF: %d", resp.StatusCode)
	}
	future := map[string]any{"schema": settingsBackupSchema, "version": 2}
	resp = f.request("POST", "/api/settings-backup/preview", map[string]any{"backup": future}, true)
	var failure struct{ Error, Code string }
	decodeBackupBody(t, resp, &failure)
	if resp.StatusCode != 400 {
		t.Fatalf("future version: %d", resp.StatusCode)
	}
	resp = f.request("POST", "/api/settings-backup/preview", map[string]any{"backup": map[string]any{"schema": "something-else", "version": 1}}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("wrong schema: %d", resp.StatusCode)
	}
	resp = f.request("POST", "/api/settings-backup/preview", map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("no backup: %d", resp.StatusCode)
	}
}

func TestSettingsBackupApplyNeedsTheCurrentPreview(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	backup := map[string]any{"schema": settingsBackupSchema, "version": 1, "macros": map[string]string{"hello": "Hi"}, "future_section": map[string]any{"x": 1}}
	status, preview := previewBackup(t, f, backup)
	if status != 200 || len(preview.Ignored) != 1 || preview.Ignored[0] != "future_section" {
		t.Fatalf("unknown sections are listed: %d %+v", status, preview)
	}
	for _, digest := range []string{"", "not-the-digest"} {
		resp := f.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": digest}, true)
		resp.Body.Close()
		if resp.StatusCode != 409 {
			t.Fatalf("digest %q: %d", digest, resp.StatusCode)
		}
	}
	// The instance changes after the preview: the same macro key now exists, so the plan is different.
	resp := f.request("PUT", "/api/macros", map[string]string{"hello": "Different"}, true)
	resp.Body.Close()
	resp = f.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": preview.Digest}, true)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("stale preview: %d", resp.StatusCode)
	}
}

func TestSettingsBackupReportsWhatCannotBeSavedAndAppliesTheRest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	registerSecondUser(t, f, "second")
	backup := map[string]any{"schema": settingsBackupSchema, "version": 1,
		"macros": map[string]string{"fine": "ok", "BAD KEY": "x"},
		"roles": []map[string]any{
			// A role named like a person would be dead weight (@second reaches the person), so the store refuses it.
			{"role_key": "second", "label": "Second", "emoji": "", "body": "x", "cardinality": "multi"},
			{"role_key": "goodrole", "label": "Good", "emoji": "", "body": "y", "cardinality": "multi"},
			{"role_key": "9bad", "label": "Bad", "emoji": "", "body": "z", "cardinality": "multi"},
		},
		"retention": map[string]int{"messages_considered_expired_after_seconds": 5},
		"ui_prefs":  map[string]string{"theme": "dark", "not_a_pref": "x"}}
	status, preview := previewBackup(t, f, backup)
	if status != 200 {
		t.Fatalf("preview: %d", status)
	}
	if got := sectionOf(preview, "macros"); got.New != 1 || len(got.Invalid) != 1 {
		t.Fatalf("macros: %+v", got)
	}
	if got := sectionOf(preview, "roles"); got.New != 2 || len(got.Invalid) != 1 || got.Invalid[0].Key != "9bad" {
		t.Fatalf("roles: %+v", got)
	}
	if got := sectionOf(preview, "retention"); len(got.Invalid) != 1 {
		t.Fatalf("retention: %+v", got)
	}
	if got := sectionOf(preview, "ui_prefs"); got.Overwrite != 1 || len(got.Invalid) != 1 {
		t.Fatalf("ui_prefs: %+v", got)
	}
	resp := f.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": preview.Digest}, true)
	var result backupResult
	decodeBackupBody(t, resp, &result)
	if result.Complete || len(result.Failed) != 1 || result.Failed[0].Key != "second" {
		t.Fatalf("result: %+v", result)
	}
	var macros map[string]string
	decodeBackupBody(t, f.request("GET", "/api/macros", nil, false), &macros)
	if macros["fine"] != "ok" {
		t.Fatalf("macros applied: %v", macros)
	}
	var roles []struct {
		Key string `json:"role_key"`
	}
	decodeBackupBody(t, f.request("GET", "/api/roles", nil, false), &roles)
	var seen []string
	for _, role := range roles {
		seen = append(seen, role.Key)
	}
	if !strings.Contains(strings.Join(seen, ","), "goodrole") || strings.Contains(strings.Join(seen, ","), "second") {
		t.Fatalf("roles after: %v", seen)
	}
}

func TestSettingsBackupSectionsCanBeChosen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	backup := map[string]any{"schema": settingsBackupSchema, "version": 1, "macros": map[string]string{"one": "1"}, "retention": map[string]int{"messages_considered_expired_after_seconds": 3600}}
	_, preview := previewBackup(t, f, backup)
	resp := f.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": preview.Digest, "sections": []string{"macros"}}, true)
	var result backupResult
	decodeBackupBody(t, resp, &result)
	if len(result.Sections) != 1 || result.Sections[0].Section != "macros" {
		t.Fatalf("only macros: %+v", result)
	}
}

// A file exported before teams and snippets were removed still reads: those sections are listed as ignored, not rejected.
func TestSettingsBackupIgnoresRemovedSections(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	backup := map[string]any{"schema": settingsBackupSchema, "version": 1, "macros": map[string]string{"one": "1"},
		"teams": []map[string]any{{"name": "duo"}}, "snippets": []map[string]string{{"name": "s", "body": "b"}}}
	status, preview := previewBackup(t, f, backup)
	if status != 200 || len(preview.Ignored) != 2 || preview.Ignored[0] != "snippets" || preview.Ignored[1] != "teams" {
		t.Fatalf("removed sections: %d %+v", status, preview.Ignored)
	}
}

// Ticking nothing on the server side applies nothing: an empty list of sections is a choice, not "all".
func TestSettingsBackupEmptySectionListAppliesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	backup := map[string]any{"schema": settingsBackupSchema, "version": 1, "macros": map[string]string{"one": "1"}, "retention": map[string]int{"messages_considered_expired_after_seconds": 7200}}
	_, preview := previewBackup(t, f, backup)
	resp := f.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": preview.Digest, "sections": []string{}}, true)
	var result backupResult
	decodeBackupBody(t, resp, &result)
	if resp.StatusCode != 200 || len(result.Sections) != 0 || !result.Complete {
		t.Fatalf("empty sections: %d %+v", resp.StatusCode, result)
	}
	var macros map[string]string
	decodeBackupBody(t, f.request("GET", "/api/macros", nil, false), &macros)
	if macros["one"] != "" {
		t.Fatalf("an empty section list saved macros: %v", macros)
	}
	var retention struct {
		Seconds int `json:"messages_considered_expired_after_seconds"`
	}
	decodeBackupBody(t, f.request("GET", "/api/retention", nil, false), &retention)
	if retention.Seconds == 7200 {
		t.Fatal("an empty section list changed the retention")
	}
}

// Model lists belong to the harnesses now: a spawn_models section in an older file is listed as ignored and changes nothing.
func TestSettingsBackupIgnoresAnOldSpawnModelsSection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	before, err := f.db.HarnessModels(t.Context(), f.harness(t, "claude", "native").ID)
	if err != nil {
		t.Fatal(err)
	}
	backup := map[string]any{"schema": settingsBackupSchema, "version": 1,
		"spawn_models": map[string]any{"claude": []map[string]string{{"id": "custom-x", "label": "Custom X"}}}}
	status, preview := previewBackup(t, f, backup)
	if status != 200 {
		t.Fatalf("preview: %d", status)
	}
	if len(preview.Ignored) != 1 || preview.Ignored[0] != "spawn_models" {
		t.Fatalf("the old model catalog is ignored with its name: %v", preview.Ignored)
	}
	resp := f.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": preview.Digest}, true)
	var result backupResult
	decodeBackupBody(t, resp, &result)
	if !result.Complete {
		t.Fatalf("apply: %+v", result)
	}
	after, err := f.db.HarnessModels(t.Context(), f.harness(t, "claude", "native").ID)
	if err != nil || len(after) != len(before) || after[0].Slug != before[0].Slug {
		t.Fatalf("the harness's models must not change: %+v then %+v (%v)", before, after, err)
	}
}

// A change to an entry the import would overwrite, made after the preview, makes the apply stale.
func TestSettingsBackupDigestCoversTheValuesBeingReplaced(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	resp := f.request("PUT", "/api/macros", map[string]string{"shared": "before"}, true)
	resp.Body.Close()
	backup := map[string]any{"schema": settingsBackupSchema, "version": 1, "macros": map[string]string{"shared": "from the file"}}
	_, preview := previewBackup(t, f, backup)
	resp = f.request("PUT", "/api/macros", map[string]string{"shared": "edited meanwhile"}, true)
	resp.Body.Close()
	resp = f.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": preview.Digest}, true)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("an edit of the value being replaced is still a stale preview: %d", resp.StatusCode)
	}
}
