package logfile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestMain(m *testing.M) {
	_, cleanup := testenv.Isolate()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAppendsToWhatIsThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.log")
	if err := os.WriteFile(path, []byte("earlier\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path, 1000, 2)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("now\n"))
	w.Close()
	if got := read(t, path); got != "earlier\nnow\n" {
		t.Fatalf("log = %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestRotatesAtTheCapAndKeepsTheNewestBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.log")
	w, err := Open(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, line := range []string{"aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n", "dddddddd\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	// Each 9-byte line fills the 10-byte file, so every line after the first starts a new file: dddddddd is current, cccccccc is .1,
	// bbbbbbbb is .2, and aaaaaaaa fell off the end.
	for name, want := range map[string]string{path: "dddddddd\n", path + ".1": "cccccccc\n", path + ".2": "bbbbbbbb\n"} {
		if got := read(t, name); got != want {
			t.Fatalf("%s = %q, want %q", filepath.Base(name), got, want)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("a third backup exists: %v", err)
	}
}

func TestOneWriteIsNeverSplitAndALargeOneStillLands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.log")
	w, err := Open(path, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	big := strings.Repeat("x", 50) + "\n"
	if n, err := w.Write([]byte(big)); err != nil || n != len(big) {
		t.Fatalf("write = %d, %v", n, err)
	}
	if got := read(t, path); got != big {
		t.Fatalf("a large write was split or lost: %q", got)
	}
}

func TestAnOversizedFileIsRotatedOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("o"), 100), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path, 50, 1)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("fresh\n"))
	w.Close()
	if read(t, path) != "fresh\n" || len(read(t, path+".1")) != 100 {
		t.Fatalf("log %q, backup %d bytes", read(t, path), len(read(t, path+".1")))
	}
}

func TestConcurrentWritersLoseNothingWithinTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.log")
	w, err := Open(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				w.Write([]byte("line\n"))
			}
		}()
	}
	wg.Wait()
	w.Close()
	if got := strings.Count(read(t, path), "\n"); got != 800 {
		t.Fatalf("%d lines, want 800", got)
	}
}

func TestRejectsBadSettingsAndClosedWrites(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "x.log"), 0, 1); err == nil {
		t.Fatal("a zero cap was accepted")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "x.log"), 10, 0); err == nil {
		t.Fatal("zero backups were accepted")
	}
	w, err := Open(filepath.Join(t.TempDir(), "x.log"), 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err := w.Write([]byte("late")); err == nil {
		t.Fatal("a write after Close succeeded")
	}
	if got := Names("a.log", 2); len(got) != 3 || got[1] != "a.log.1" || got[2] != "a.log.2" {
		t.Fatalf("names %v", got)
	}
}

func TestAppendOnlyNeverRotatesUntilEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aimebu.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("o"), 100), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := OpenAppendOnly(path, 50, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Past its cap already, and the writer still only appends: a process that may be about to lose the race to own the log must not
	// move the file under the one that owns it.
	w.Write([]byte("first\n"))
	w.Write([]byte(strings.Repeat("y", 60) + "\n"))
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("rotated before it was enabled: %v", err)
	}
	if got := read(t, path); !strings.HasPrefix(got, strings.Repeat("o", 100)+"first\n") {
		t.Fatalf("appended content lost: %d bytes", len(got))
	}
	if err := w.EnableRotation(); err != nil {
		t.Fatal(err)
	}
	// Enabling rotates what is over the cap at once, and the next write goes to a fresh file.
	if info, err := os.Stat(path + ".1"); err != nil || info.Size() < 100 {
		t.Fatalf("the oversized file was not rotated on enabling: %v %v", info, err)
	}
	w.Write([]byte("after\n"))
	if got := read(t, path); got != "after\n" {
		t.Fatalf("log after enabling = %q", got)
	}
}
