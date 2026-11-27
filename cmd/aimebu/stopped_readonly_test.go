package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
)

// treeSnapshot maps every entry under root (files by content hash, directories by name) so a test can show that a command changed nothing
// and created nothing.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			out[rel] = "dir"
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out[rel] = strconv.Itoa(len(raw)) + ":" + hashHex(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func hashHex(raw []byte) string {
	sum := sha256.Sum256(raw)
	const digits = "0123456789abcdef"
	buf := make([]byte, 0, len(sum)*2)
	for _, b := range sum {
		buf = append(buf, digits[b>>4], digits[b&15])
	}
	return string(buf)
}

func requireTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := treeSnapshot(t, root)
	if reflect.DeepEqual(before, after) {
		return
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("created %s", name)
		}
	}
	for name, sum := range before {
		if got, ok := after[name]; !ok {
			t.Errorf("removed %s", name)
		} else if got != sum {
			t.Errorf("changed %s", name)
		}
	}
	t.FailNow()
}

// v1RootFixture makes a stopped v1 installation: v1's database lives at server/aimebu.sqlite, in rollback-journal mode, with no lock file.
func v1RootFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE schema_meta(version INTEGER); INSERT INTO schema_meta VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func stoppedEnv(t *testing.T, root string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("AIMEBU_BIND", "127.0.0.1")
	t.Setenv("AIMEBU_PORT", strconv.Itoa(freeTCPPorts(t, 1)[0]))
	t.Setenv("AIMEBU_ALLOW", "")
	t.Setenv("AIMEBU_TLS_CERT", "")
	t.Setenv("AIMEBU_TLS_KEY", "")
}

func doctorDatabaseCheck(t *testing.T) doctorCheck {
	t.Helper()
	var out bytes.Buffer
	doctorCommand(context.Background(), []string{"--json"}, &out, io.Discard)
	var result doctorResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, check := range result.Checks {
		if check.Name == "sqlite database" {
			return check
		}
	}
	t.Fatal("database finding missing")
	return doctorCheck{}
}

func TestDoctorStoppedV1RootWritesNothing(t *testing.T) {
	root := v1RootFixture(t)
	stoppedEnv(t, root)
	before := treeSnapshot(t, root)
	check := doctorDatabaseCheck(t)
	requireTreeUnchanged(t, root, before)
	if check.Status != statusWarn || !strings.Contains(check.Description, "v1") || strings.Contains(check.Description, "v1 server was started") {
		t.Fatalf("database finding = %+v", check)
	}
}

// A v2 database that also holds v1's real schema_meta(version)=1, with the backup in place, is the mixed case: doctor must say so, not
// call it a v1 root awaiting cutover.
func TestDoctorStoppedMixedDatabaseIsTheMixedFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "server", "aimebu.sqlite")
	createFixtureDatabase(t, path)
	addV1SchemaMeta(t, path)
	if err := os.Mkdir(filepath.Join(root, v1BackupName), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", ".writer.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	stoppedEnv(t, root)
	before := treeSnapshot(t, root)
	check := doctorDatabaseCheck(t)
	requireTreeUnchanged(t, root, before)
	if check.Status != statusFail || !strings.Contains(check.Description, "restore the backup into a new folder") {
		t.Fatalf("database finding = %+v", check)
	}
}

func TestSessionsStoppedV1RootWritesNothing(t *testing.T) {
	root := v1RootFixture(t)
	stoppedEnv(t, root)
	before := treeSnapshot(t, root)
	var out, errOut bytes.Buffer
	sessionsCommand(context.Background(), nil, &out, &errOut)
	requireTreeUnchanged(t, root, before)
}

func TestDoctorStoppedOlderSchemaIsNotMigrated(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "server", "aimebu.sqlite")
	createFixtureDatabase(t, path)
	rewriteSchemaVersion(t, path, 1)
	stoppedEnv(t, root)
	// A v2 root has the lock file its server made at its first start; the fixture is a bare database.
	if err := os.WriteFile(filepath.Join(root, "server", ".writer.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, root)
	check := doctorDatabaseCheck(t)
	requireTreeUnchanged(t, root, before)
	if check.Status != statusWarn || !strings.Contains(check.Description, "forward-migrates") {
		t.Fatalf("database finding = %+v", check)
	}
}

func TestSessionsStoppedV2WritesNothing(t *testing.T) {
	root := t.TempDir()
	createFixtureDatabase(t, filepath.Join(root, "server", "aimebu.sqlite"))
	stoppedEnv(t, root)
	// The first run may create the lock file a stopped v2 root takes its read under; after that nothing changes.
	sessionsCommand(context.Background(), nil, io.Discard, io.Discard)
	before := treeSnapshot(t, root)
	var out, errOut bytes.Buffer
	if code := sessionsCommand(context.Background(), nil, &out, &errOut); code != 0 || errOut.Len() != 0 {
		t.Fatalf("sessions exit=%d stderr=%s", code, errOut.String())
	}
	requireTreeUnchanged(t, root, before)
}

func TestDoctorStoppedV2WritesNothing(t *testing.T) {
	root := t.TempDir()
	createFixtureDatabase(t, filepath.Join(root, "server", "aimebu.sqlite"))
	stoppedEnv(t, root)
	doctorDatabaseCheck(t)
	before := treeSnapshot(t, root)
	if check := doctorDatabaseCheck(t); check.Status != statusOK {
		t.Fatalf("database finding = %+v", check)
	}
	requireTreeUnchanged(t, root, before)
}
