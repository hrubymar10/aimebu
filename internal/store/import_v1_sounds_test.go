package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
)

func mp3Bytes(extra int) []byte {
	return append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), make([]byte, extra)...)
}
func wavBytes() []byte { return []byte("RIFF\x24\x00\x00\x00WAVEfmt ") }

// v1SoundFixture adds a sounds table and files to the shared v1 fixture.
func v1SoundFixture(t *testing.T) (string, V1ImportMap) {
	t.Helper()
	root, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE sounds(id TEXT PRIMARY KEY,data TEXT)"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "server", "sounds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	add := func(id, name, ext string, size int64, content []byte) {
		t.Helper()
		raw, _ := json.Marshal(v1ImportSound{UUID: id, Name: name, Size: size, UploadedAt: "2026-10-01T12:00:00Z", Ext: ext})
		if _, err := db.Exec("INSERT INTO sounds(id,data) VALUES (?,?)", id, string(raw)); err != nil {
			t.Fatal(err)
		}
		if content != nil {
			fileExt := ext
			if fileExt == "" {
				fileExt = "mp3"
			}
			if err := os.WriteFile(filepath.Join(dir, id+"."+fileExt), content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("good-mp3", "ping.mp3", "mp3", 10, mp3Bytes(0))
	add("good-wav", "bell.wav", "wav", 20, wavBytes())
	add("legacy", "old.mp3", "", 10, mp3Bytes(4))
	add("bad-header", "fake.mp3", "mp3", 9, []byte("not audio"))
	add("missing", "gone.mp3", "mp3", 10, nil)
	add("huge", "big.mp3", "mp3", 2<<20, mp3Bytes(MaxSoundFileSize))
	add("ogg", "song.ogg", "ogg", 4, []byte("OggS"))
	add("../escape", "evil.mp3", "mp3", 10, nil)
	return root, mapping
}

func soundFiles(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(path), "sounds"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestV1ImportCarriesSoundsAndReportsTheRest(t *testing.T) {
	t.Parallel()
	source, mapping := v1SoundFixture(t)
	s, path := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil || preview.Counts["sounds"] != 8 {
		t.Fatalf("preview sounds=%d err=%v", preview.Counts["sounds"], err)
	}

	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil || dry.Imported["sounds"] != 3 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sound`).Scan(&count); err != nil || count != 0 || len(soundFiles(t, path)) != 0 {
		t.Fatalf("dry run wrote sounds: rows=%d files=%v err=%v", count, soundFiles(t, path), err)
	}

	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["sounds"] != 3 || report.Skipped["sounds"] != 5 || len(report.SkippedSounds) != 5 {
		t.Fatalf("import: %+v %v", report, err)
	}
	for id, want := range map[string]string{
		"bad-header": "not a valid .mp3", "missing": "missing from the backup", "huge": "between 1 byte and 1 MB",
		"ogg": "only .mp3 and .wav", "(unusable id #1)": "cannot name a file",
	} {
		if !strings.Contains(report.SkippedSounds[id], want) {
			t.Fatalf("skipped %q = %q, want %q", id, report.SkippedSounds[id], want)
		}
	}
	sounds, err := s.ListSounds(ctx)
	if err != nil || len(sounds) != 3 {
		t.Fatalf("sounds=%+v err=%v", sounds, err)
	}
	names := map[string]string{}
	for _, sound := range sounds {
		names[sound.Name] = sound.Ext
		_, file, err := s.SoundFile(ctx, sound.ID)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := os.ReadFile(file)
		if err != nil || int64(len(stored)) != sound.Size {
			t.Fatalf("stored file for %s: %d bytes, err=%v", sound.Name, len(stored), err)
		}
	}
	if names["ping.mp3"] != "mp3" || names["bell.wav"] != "wav" || names["old.mp3"] != "mp3" {
		t.Fatalf("names=%v", names)
	}
	if got := soundFiles(t, path); len(got) != 3 {
		t.Fatalf("files=%v, want exactly the three imported ones", got)
	}
	var owner string
	if err := s.db.QueryRow(`SELECT DISTINCT created_by FROM sound`).Scan(&owner); err != nil || owner != "usr_owner" {
		t.Fatalf("created_by=%q err=%v", owner, err)
	}
	if want := mp3Bytes(4); !bytes.Contains(mustRead(t, s, ctx, "old.mp3"), want[:3]) {
		t.Fatal("legacy sound content lost")
	}

	// A rerun imports nothing, and a sound the owner deleted stays deleted.
	if err := s.DeleteSound(ctx, sounds[0].ID); err != nil {
		t.Fatal(err)
	}
	again, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || again.Imported["sounds"] != 0 || again.AlreadyPresent["sounds"] != 3 {
		t.Fatalf("rerun: %+v %v", again, err)
	}
	if left, _ := s.ListSounds(ctx); len(left) != 2 {
		t.Fatalf("deleted sound came back: %+v", left)
	}
}

func mustRead(t *testing.T, s *Store, ctx context.Context, name string) []byte {
	t.Helper()
	sounds, _ := s.ListSounds(ctx)
	for _, sound := range sounds {
		if sound.Name == name {
			_, file, _ := s.SoundFile(ctx, sound.ID)
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
	t.Fatalf("no sound named %s", name)
	return nil
}

func TestV1ImportSoundsRespectTheCountCapAlreadyUsedInV2(t *testing.T) {
	t.Parallel()
	source, mapping := v1SoundFixture(t)
	s, path := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	// v2 already holds nine sounds: only one more fits.
	for i := 0; i < MaxSoundFiles-1; i++ {
		if _, err := s.db.Exec(`INSERT INTO sound(id,name,ext,size,created_by,created_at) VALUES (?,?,?,?,?,?)`,
			fmt.Sprintf("snd_pre%d", i), fmt.Sprintf("pre%d.mp3", i), "mp3", 1000, "usr_owner", timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["sounds"] != 1 || report.Skipped["sounds"] != 7 {
		t.Fatalf("count cap: %+v %v", report, err)
	}
	overCount := 0
	for _, reason := range report.SkippedSounds {
		if strings.Contains(reason, "at most 10 custom sounds") {
			overCount++
		}
	}
	if overCount != 2 || len(soundFiles(t, path)) != 1 {
		t.Fatalf("over-cap skips=%d files=%v reasons=%v", overCount, soundFiles(t, path), report.SkippedSounds)
	}
}

func TestV1ImportWithoutSoundsTableIsFine(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Imported["sounds"] != 0 || len(report.SkippedSounds) != 0 {
		t.Fatalf("no sounds table: %+v %v", report, err)
	}
}

// addRawSound inserts a sounds row (and optionally its file) into a fixture.
func addRawSound(t *testing.T, root, id, data string, content []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS sounds(id TEXT PRIMARY KEY,data TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO sounds(id,data) VALUES (?,?)", id, data); err != nil {
		t.Fatal(err)
	}
	if content != nil {
		dir := filepath.Join(root, "server", "sounds")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".mp3"), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func soundRow(id, name, uploadedAt string) string {
	raw, _ := json.Marshal(v1ImportSound{UUID: id, Name: name, UploadedAt: uploadedAt, Ext: "mp3"})
	return string(raw)
}

func ownerStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, path := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestV1ImportSkipsMalformedSoundRowsInsteadOfGuessing(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	// A readable file exists for the id, so a guess would have imported it.
	addRawSound(t, source, "broken", `{"uuid":`, mp3Bytes(0))
	s, path := ownerStore(t)
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Imported["sounds"] != 0 || report.Skipped["sounds"] != 1 || !strings.Contains(report.SkippedSounds["broken"], "could not be read") {
		t.Fatalf("report: %+v %v", report, err)
	}
	if got := soundFiles(t, path); len(got) != 0 {
		t.Fatalf("malformed row left files: %v", got)
	}
}

func TestV1ImportKeepsTheOldestSoundsWhenThePoolIsFull(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	// Ids sort the other way round from upload time on purpose; an entry with
	// no readable time comes last.
	addRawSound(t, source, "zzz-oldest", soundRow("zzz-oldest", "oldest.mp3", "2026-10-01T09:00:00Z"), mp3Bytes(0))
	addRawSound(t, source, "mmm-middle", soundRow("mmm-middle", "middle.mp3", "2026-10-02T09:00:00+02:00"), mp3Bytes(1))
	addRawSound(t, source, "aaa-newest", soundRow("aaa-newest", "newest.mp3", "2026-10-03T09:00:00Z"), mp3Bytes(2))
	addRawSound(t, source, "000-no-time", soundRow("000-no-time", "undated.mp3", "garbage"), mp3Bytes(3))
	s, _ := ownerStore(t)
	for i := 0; i < MaxSoundFiles-2; i++ {
		if _, err := s.db.Exec(`INSERT INTO sound(id,name,ext,size,created_by,created_at) VALUES (?,?,?,?,?,?)`,
			fmt.Sprintf("snd_pre%d", i), fmt.Sprintf("pre%d.mp3", i), "mp3", 1000, "usr_owner", timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Imported["sounds"] != 2 || report.Skipped["sounds"] != 2 {
		t.Fatalf("report: %+v %v", report, err)
	}
	names := map[string]bool{}
	sounds, _ := s.ListSounds(context.Background())
	for _, sound := range sounds {
		names[sound.Name] = true
	}
	if !names["oldest.mp3"] || !names["middle.mp3"] || names["newest.mp3"] || names["undated.mp3"] {
		t.Fatalf("kept the wrong sounds: %v", names)
	}
}

func TestV1ImportNumbersEachUnusableSoundID(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	addRawSound(t, source, "../one", soundRow("../one", "one.mp3", "2026-10-01T09:00:00Z"), nil)
	addRawSound(t, source, "../two", soundRow("../two", "two.mp3", "2026-10-02T09:00:00Z"), nil)
	s, _ := ownerStore(t)
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Skipped["sounds"] != 2 || len(report.SkippedSounds) != 2 {
		t.Fatalf("skips must be reported one by one: %+v %v", report, err)
	}
}

func TestV1ImportSoundThatIsNotARegularFileSaysSo(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	addRawSound(t, source, "pipe", soundRow("pipe", "pipe.mp3", "2026-10-01T09:00:00Z"), nil)
	dir := filepath.Join(source, "server", "sounds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.mp3"), 0o600); err != nil {
		t.Skipf("no fifo support: %v", err)
	}
	s, _ := ownerStore(t)
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Skipped["sounds"] != 1 || !strings.Contains(report.SkippedSounds["pipe"], "not a regular file") {
		t.Fatalf("report: %+v %v", report, err)
	}
}
