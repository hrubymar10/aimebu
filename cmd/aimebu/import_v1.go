package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/configroot"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func lockServerDirectory(dir string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(dir, ".writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// `aimebu server status`, stop and start look for a running server by taking this lock for an instant, so a refusal is tried a few
	// more times before it is believed: a server that is another's loses after a quarter of a second, not on a probe's microsecond.
	var err2 error
	for attempt := 0; attempt < 10; attempt++ {
		if err2 = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err2 == nil {
			return file, nil
		}
		if !errors.Is(err2, syscall.EWOULDBLOCK) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = file.Close()
	return nil, fmt.Errorf("server database is in use by a running v2 server; stop it first (aimebu server stop, or where it runs): %w", err2)
}

func unlockServerDirectory(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func copyImportFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	return err
}

func importV1Command(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("import-v1", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sourceDir := flags.String("from", "", "copied v1 config directory")
	mapPath := flags.String("map", "", "JSON mapping file")
	targetRoot := flags.String("to", "", "v2 config directory")
	dryRun := flags.Bool("dry-run", false, "validate and report without database changes")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *sourceDir == "" || *mapPath == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aimebu import-v1 --from <copied-v1-dir> --map <file> [--to <v2-config-dir>] [--dry-run]")
		return 2
	}
	root := *targetRoot
	if root == "" {
		var err error
		root, err = configroot.FromEnv()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if err := refuseV1RootWrite(root); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	mapping, err := store.ReadV1ImportMap(*mapPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	dir := filepath.Join(root, "server")
	if _, err := os.Stat(dir); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	lock, err := lockServerDirectory(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer unlockServerDirectory(lock)
	database := filepath.Join(dir, "aimebu.sqlite")
	if _, err := os.Stat(database); err != nil {
		fmt.Fprintf(stderr, "v2 destination database must already exist with an owner: %v\n", err)
		return 1
	}
	if *dryRun {
		scratch, err := os.MkdirTemp("", "aimebu-v1-import-*")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer os.RemoveAll(scratch)
		scratchDir := filepath.Join(scratch, "server")
		if err := os.MkdirAll(scratchDir, 0700); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		scratchDB := filepath.Join(scratchDir, "aimebu.sqlite")
		if err := copyImportFile(database, scratchDB); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for _, ext := range []string{"-wal", "-shm"} {
			err := copyImportFile(database+ext, scratchDB+ext)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		database = scratchDB
	}
	destination, err := store.Open(ctx, database, store.Options{})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer destination.Close()
	report, err := destination.ImportV1(ctx, *sourceDir, mapping, *dryRun)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return importWarningStatus(report, *dryRun, stderr)
}

// importWarningStatus is 0 for a clean import. A room or memory scope with no project is not imported; the report says so, but a script
// that only checks the exit status would miss it, so this is a warning and a distinct non-zero status (the import itself, or the dry
// run, went through).
func importWarningStatus(report store.V1ImportReport, dryRun bool, stderr io.Writer) int {
	if len(report.SkippedNoProject) == 0 {
		return 0
	}
	verb := "were not imported"
	if dryRun {
		verb = "would not be imported"
	}
	fmt.Fprintf(stderr, "warning: %d v1 room(s) have no project in the map and %s: %s\n", len(report.SkippedNoProject), verb, strings.Join(report.SkippedNoProject, ", "))
	return exitImportDropped
}

// exitImportDropped is the status of a finished import (or dry run) that left rooms out for want of a project.
const exitImportDropped = 3
