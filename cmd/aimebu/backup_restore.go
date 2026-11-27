package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

type archiveFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type archiveManifest struct {
	Version            string        `json:"version"`
	CreatedAt          string        `json:"created_at"`
	SchemaVersion      int           `json:"schema_version"`
	SourceRoot         string        `json:"source_root,omitempty"`
	Files              []archiveFile `json:"files"`
	MissingAttachments []string      `json:"missing_attachments,omitempty"`
	SkippedJournals    []string      `json:"skipped_journals,omitempty"`
}

func backupCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("out", "", "archive path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aimebu backup [--out <file>]")
		return 2
	}
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	root, err := safePruneRoot(config.root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := refuseV1RootWrite(root); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *out == "" {
		name := strings.Fields(resolveVersion())
		v := "v2"
		if len(name) > 1 {
			v = name[1]
		}
		*out = filepath.Join(root, "backups", "aimebu-"+v+"-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".tar.gz")
	}
	if err := makeBackup(ctx, config, root, *out, stdout); err != nil {
		fmt.Fprintln(stderr, "backup:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Backup saved to %s (mode 0600). It contains secrets, including keys.\n", *out)
	return 0
}

func makeBackup(ctx context.Context, config serverConfig, root, output string, stdout io.Writer) (err error) {
	return makeBackupWithSnapshotHook(ctx, config, root, output, stdout, nil)
}

func makeBackupWithSnapshotHook(ctx context.Context, config serverConfig, root, output string, stdout io.Writer, afterSnapshot func()) (err error) {
	if err = os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(output); err == nil {
		return fmt.Errorf("output already exists: %s", output)
	} else if !os.IsNotExist(err) {
		return err
	}
	// Reserve the destination before walking, so it cannot be included in itself.
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(output)
		}
	}()
	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return err
	}
	backupInfo, err := os.Lstat(backupDir)
	if err != nil {
		return err
	}
	if !backupInfo.IsDir() || backupInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe backup directory %s", backupDir)
	}
	if err := os.Chmod(backupDir, 0700); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(backupDir, ".tmp-backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	snapshot := filepath.Join(scratch, "aimebu.sqlite")
	dbPath := filepath.Join(root, "server", "aimebu.sqlite")
	var lock *os.File
	serverUp, err := serverRunning(root)
	if err != nil {
		return err
	}
	if !serverUp {
		lock, err = pruneLock(filepath.Join(root, "server", ".writer.lock"), "server")
		if err != nil { // The server may have started between the probe and lock.
			lockErr := err
			serverUp, err = serverRunning(root)
			if err != nil {
				return err
			}
			if !serverUp {
				return fmt.Errorf("server lock: %w", lockErr)
			}
		} else {
			defer unlockServerDirectory(lock)
		}
	}
	if _, statErr := os.Stat(dbPath); statErr == nil || serverUp {
		if serverUp {
			if err = fetchServerSnapshot(ctx, config, snapshot); err != nil {
				return err
			}
		} else if err = snapshotStoppedDatabase(ctx, lock, dbPath, snapshot); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if afterSnapshot != nil {
		afterSnapshot()
	}
	manifest := archiveManifest{Version: resolveVersion(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), SourceRoot: root, Files: []archiveFile{}}
	attachmentPaths := map[string]string{}
	if _, statErr := os.Stat(snapshot); statErr == nil {
		manifest.SchemaVersion, err = snapshotSchema(ctx, snapshot)
		if err != nil {
			return err
		}
		attachmentPaths, err = store.SnapshotAttachments(ctx, snapshot)
		if err != nil {
			return err
		}
	}
	// Hold the free journal locks while copying. A live journal is skipped: its
	// owner alone may open it, and copying its WAL files would be inconsistent.
	journalLocks := []*os.File{}
	defer func() {
		for _, held := range journalLocks {
			unlockServerDirectory(held)
		}
	}()
	skipped := map[string]bool{}
	for _, journal := range supervisor.InstanceJournals(filepath.Join(root, "supervisor")) {
		l, lockErr := pruneLock(journal+".lock", "supervisor")
		if lockErr != nil {
			if held, _, _, probeErr := supervisor.JournalLockInfo(journal); probeErr == nil && held {
				rel, _ := filepath.Rel(root, journal)
				skipped[filepath.ToSlash(rel)] = true
				manifest.SkippedJournals = append(manifest.SkippedJournals, filepath.ToSlash(rel))
				continue
			}
			return lockErr
		}
		journalLocks = append(journalLocks, l)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	defer func() {
		if !complete {
			tarWriter.Close()
			gzipWriter.Close()
		}
	}()
	write := func(name, source string, mode int64) error {
		f, openErr := os.Open(source)
		if openErr != nil {
			return openErr
		}
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe source file %s", source)
		}
		header := &tar.Header{Name: name, Mode: mode, Size: info.Size(), Typeflag: tar.TypeReg, ModTime: info.ModTime()}
		if writeErr := tarWriter.WriteHeader(header); writeErr != nil {
			return writeErr
		}
		hash := sha256.New()
		if _, copyErr := io.CopyN(io.MultiWriter(tarWriter, hash), f, info.Size()); copyErr != nil {
			return copyErr
		}
		manifest.Files = append(manifest.Files, archiveFile{Path: name, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: info.Size()})
		return nil
	}
	if _, statErr := os.Stat(snapshot); statErr == nil {
		if err = write("server/aimebu.sqlite", snapshot, 0600); err != nil {
			return err
		}
	}
	err = filepath.WalkDir(root, func(p string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		base := entry.Name()
		if entry.IsDir() {
			if base == "backups" || base == "tmp" || base == "temp" {
				return filepath.SkipDir
			}
			if name == "server/attachments" {
				return filepath.SkipDir
			}
			return nil
		}
		if name == "server/aimebu.sqlite" || name == "server/aimebu.sqlite-wal" || name == "server/aimebu.sqlite-shm" || name == "server/doctor.key" || name == "server/aimebu.pid" || strings.HasSuffix(base, ".lock") || strings.HasSuffix(base, ".log") || strings.HasSuffix(base, ".tmp") || strings.HasPrefix(base, ".backup-snapshot-") {
			return nil
		}
		for journal := range skipped {
			if name == journal || name == journal+"-wal" || name == journal+"-shm" || name == journal+"-journal" {
				return nil
			}
		}
		if name == filepath.ToSlash(strings.TrimPrefix(output, root+string(filepath.Separator))) && strings.HasPrefix(output, root+string(filepath.Separator)) {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in data root: %s", name)
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file in data root: %s", name)
		}
		return write(name, p, int64(info.Mode().Perm()))
	})
	if err != nil {
		return err
	}
	attachmentIDs := make([]string, 0, len(attachmentPaths))
	for id := range attachmentPaths {
		attachmentIDs = append(attachmentIDs, id)
	}
	sort.Strings(attachmentIDs)
	writtenAttachments := map[string]bool{}
	for _, id := range attachmentIDs {
		attachmentPath := attachmentPaths[id]
		rel, relErr := filepath.Rel(root, attachmentPath)
		if relErr != nil || !filepath.IsAbs(attachmentPath) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !strings.HasPrefix(filepath.ToSlash(rel), "server/attachments/") {
			return fmt.Errorf("attachment %s has path outside the backup attachments folder: %s", id, attachmentPath)
		}
		info, statErr := os.Lstat(attachmentPath)
		if os.IsNotExist(statErr) {
			manifest.MissingAttachments = append(manifest.MissingAttachments, id)
			continue
		}
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe attachment file %s", attachmentPath)
		}
		if writtenAttachments[rel] {
			continue
		}
		if err := write(filepath.ToSlash(rel), attachmentPath, int64(info.Mode().Perm())); err != nil {
			if os.IsNotExist(err) {
				manifest.MissingAttachments = append(manifest.MissingAttachments, id)
				continue
			}
			return err
		}
		writtenAttachments[rel] = true
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err = tarWriter.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(encoded)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err = tarWriter.Write(encoded); err != nil {
		return err
	}
	if err = tarWriter.Close(); err != nil {
		return err
	}
	if err = gzipWriter.Close(); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	complete = true
	if len(manifest.SkippedJournals) > 0 {
		fmt.Fprintf(stdout, "Skipped %d running supervisor journal(s); their owner must snapshot them.\n", len(manifest.SkippedJournals))
	}
	if len(manifest.MissingAttachments) > 0 {
		fmt.Fprintf(stdout, "Warning: %d attachment(s) missing from the snapshot: %s.\n", len(manifest.MissingAttachments), strings.Join(manifest.MissingAttachments, ", "))
	}
	return nil
}

// snapshotStoppedDatabase snapshots the live database of a stopped server. lock is the server's startup writer lock, which the caller took
// first; without it a server could start and open the same file while this process reads it.
func snapshotStoppedDatabase(ctx context.Context, lock *os.File, source, dest string) error {
	if lock == nil {
		return errors.New("refusing to open the server database without the server lock")
	}
	if err := store.VacuumFileInto(ctx, source, dest); err != nil {
		return err
	}
	return os.Chmod(dest, 0600)
}

func fetchServerSnapshot(ctx context.Context, config serverConfig, dest string) error {
	key, check, ok := readDoctorKey(config.root)
	if !ok {
		return errors.New(check.Description)
	}
	base := "http://" + net.JoinHostPort(config.bind, strconv.Itoa(config.port))
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, base+"/api/backup/database", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := doctorHTTPClient(0)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server snapshot request: HTTP %d (restart the server from this build)", resp.StatusCode)
	}
	file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, resp.Body)
	return errors.Join(err, file.Close())
}

// snapshotSchema reads the schema version of a private copy: the snapshot taken for an archive or the database extracted from one.
func snapshotSchema(ctx context.Context, p string) (int, error) {
	return store.FileSchemaVersion(ctx, p)
}

func restoreCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(stderr)
	yes := flags.Bool("yes", false, "skip confirmation")
	// Allow --yes after the archive path too.
	var file string
	for _, arg := range args {
		if arg == "--yes" {
			*yes = true
		} else if file == "" && !strings.HasPrefix(arg, "-") {
			file = arg
		} else {
			fmt.Fprintln(stderr, "usage: aimebu restore <file> [--yes]")
			return 2
		}
	}
	if file == "" {
		fmt.Fprintln(stderr, "usage: aimebu restore <file> [--yes]")
		return 2
	}
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	root, err := safePruneRoot(config.root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := restoreArchive(ctx, root, file, *yes, stdout); err != nil {
		fmt.Fprintln(stderr, "restore:", err)
		return 1
	}
	return 0
}

func restoreArchive(ctx context.Context, root, archive string, yes bool, stdout io.Writer) error {
	if running, err := serverRunning(root); err != nil {
		return err
	} else if running {
		return errors.New("server is running; stop it with `aimebu server stop --with-supervisor` or `brew services stop aimebu`")
	}
	if held, err := heldSupervisors(root); err != nil {
		return err
	} else if len(held) > 0 {
		return errors.New("supervisor is running; stop it with `aimebu server stop --with-supervisor` or `brew services stop aimebu`")
	}
	stage, err := os.MkdirTemp(filepath.Dir(root), ".aimebu-restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	manifest, err := extractVerified(archive, stage)
	if err != nil {
		return err
	}
	if manifest.SchemaVersion > store.LatestSchemaVersion() {
		return fmt.Errorf("archive schema v%d is newer than this binary supports (v%d)", manifest.SchemaVersion, store.LatestSchemaVersion())
	}
	copyPath := filepath.Join(stage, "server", "aimebu.sqlite")
	if _, err := os.Stat(copyPath); err == nil {
		actual, schemaErr := snapshotSchema(ctx, copyPath)
		if schemaErr != nil {
			return schemaErr
		}
		if actual != manifest.SchemaVersion {
			return fmt.Errorf("manifest schema v%d does not match database schema v%d", manifest.SchemaVersion, actual)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if manifest.SchemaVersion != 0 {
		return errors.New("manifest names a database schema but the archive has no database")
	}
	if !yes {
		tty, ttyErr := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if ttyErr != nil {
			return errors.New("confirmation needs a TTY; rerun with --yes")
		}
		defer tty.Close()
		fmt.Fprintf(tty, "Replace %s with the verified backup? The original will be kept. [y/N] ", root)
		answer, _ := bufio.NewReader(tty).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(answer)) != "y" {
			return errors.New("restore cancelled")
		}
	}
	// Recheck after confirmation, and hold the existing locks until replacement.
	if err := os.MkdirAll(filepath.Join(root, "server"), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "supervisor"), 0700); err != nil {
		return err
	}
	serverLock, err := pruneLock(filepath.Join(root, "server", ".writer.lock"), "server")
	if err != nil {
		return fmt.Errorf("server is running; stop it with `aimebu server stop --with-supervisor` or `brew services stop aimebu`: %w", err)
	}
	defer unlockServerDirectory(serverLock)
	journals := supervisor.InstanceJournals(filepath.Join(root, "supervisor"))
	legacy := supervisorJournalPath(root)
	foundLegacy := false
	for _, journal := range journals {
		if journal == legacy {
			foundLegacy = true
		}
	}
	if !foundLegacy {
		journals = append(journals, legacy)
	}
	journalLocks := []*os.File{}
	defer func() {
		for _, held := range journalLocks {
			unlockServerDirectory(held)
		}
	}()
	for _, journal := range journals {
		locked, lockErr := pruneLock(journal+".lock", "supervisor")
		if lockErr != nil {
			return fmt.Errorf("supervisor is running; stop it with `aimebu server stop --with-supervisor` or `brew services stop aimebu`: %w", lockErr)
		}
		journalLocks = append(journalLocks, locked)
	}
	old := root + ".pre-restore-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(root, old); err != nil {
		return err
	}
	if err := os.Rename(stage, root); err != nil {
		_ = os.Rename(old, root)
		return err
	}
	fmt.Fprintf(stdout, "Restored %s. Original data kept at %s. Start the server to apply any older schema migrations.\n", root, old)
	if len(manifest.MissingAttachments) > 0 {
		fmt.Fprintf(stdout, "Warning: %d attachment(s) were missing when backed up: %s.\n", len(manifest.MissingAttachments), strings.Join(manifest.MissingAttachments, ", "))
	}
	return nil
}

func extractVerified(archive, stage string) (archiveManifest, error) {
	var manifest archiveManifest
	file, err := os.Open(archive)
	if err != nil {
		return manifest, err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return manifest, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]archiveFile{}
	manifestSeen := false
	for {
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return manifest, nextErr
		}
		name := h.Name
		if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return manifest, fmt.Errorf("unsafe archive path %q", name)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return manifest, fmt.Errorf("archive contains a non-regular entry: %s", name)
		}
		if _, exists := seen[name]; exists || name == "manifest.json" && manifestSeen {
			return manifest, fmt.Errorf("duplicate archive path %s", name)
		}
		if name == "manifest.json" {
			if h.Size > 16<<20 {
				return manifest, errors.New("manifest too large")
			}
			data, readErr := io.ReadAll(tr)
			if readErr != nil {
				return manifest, readErr
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return manifest, err
			}
			manifestSeen = true
			continue
		}
		dest := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return manifest, err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0777)
		if err != nil {
			return manifest, err
		}
		hash := sha256.New()
		size, err := io.Copy(io.MultiWriter(out, hash), tr)
		err = errors.Join(err, out.Close())
		if err != nil {
			return manifest, err
		}
		seen[name] = archiveFile{Path: name, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size}
	}
	if !manifestSeen {
		return manifest, errors.New("archive has no manifest.json")
	}
	if manifest.Version == "" || manifest.SchemaVersion < 0 {
		return manifest, errors.New("invalid archive manifest")
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt); err != nil {
		return manifest, fmt.Errorf("invalid archive time: %w", err)
	}
	if len(manifest.Files) != len(seen) {
		return manifest, errors.New("manifest file list does not match archive")
	}
	listed := map[string]bool{}
	for _, entry := range manifest.Files {
		actual, ok := seen[entry.Path]
		if !ok || listed[entry.Path] || entry.SHA256 != actual.SHA256 || entry.Size != actual.Size {
			return manifest, fmt.Errorf("checksum or file list mismatch: %s", entry.Path)
		}
		listed[entry.Path] = true
	}
	return manifest, nil
}
