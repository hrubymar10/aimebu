package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/store"
)

func TestImportWriterLockExcludesServer(t *testing.T) {
	dir := t.TempDir()
	first, err := lockServerDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".writer.lock")); err != nil {
		t.Fatalf("dry-run must create its writer lock: %v", err)
	}
	if second, err := lockServerDirectory(dir); err == nil {
		unlockServerDirectory(second)
		t.Fatal("second writer acquired a held server lock")
	}
	unlockServerDirectory(first)
	third, err := lockServerDirectory(dir)
	if err != nil {
		t.Fatalf("lock remained held after release: %v", err)
	}
	unlockServerDirectory(third)
}

func TestImportWarnsAndExitsNonZeroWhenRoomsHaveNoProject(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if code := importWarningStatus(store.V1ImportReport{}, false, &out); code != 0 || out.Len() != 0 {
		t.Fatalf("a clean import is silent and 0: %d %q", code, out.String())
	}
	report := store.V1ImportReport{SkippedNoProject: []string{"stray", "work"}}
	if code := importWarningStatus(report, false, &out); code != exitImportDropped || !strings.Contains(out.String(), "2 v1 room(s) have no project") || !strings.Contains(out.String(), "were not imported: stray, work") {
		t.Fatalf("an import that dropped rooms: %d %q", code, out.String())
	}
	out.Reset()
	if code := importWarningStatus(report, true, &out); code != exitImportDropped || !strings.Contains(out.String(), "would not be imported") {
		t.Fatalf("a dry run says what would happen: %d %q", code, out.String())
	}
}
