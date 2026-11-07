package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped default is valid, points at the EU ingest host, and is what makes the popup's toggle start on.
func TestShippedDefaultDSN(t *testing.T) {
	if err := ValidDSN(DefaultDSN); err != nil {
		t.Fatalf("the shipped default DSN is not valid: %v", err)
	}
	if !strings.Contains(DefaultDSN, ".ingest.de.sentry.io/") {
		t.Fatalf("the default DSN must use the EU ingest host: %q", DefaultDSN)
	}
	if !(Stored{}).EnabledOrDefault() || (Stored{}).DSNOrDefault() != DefaultDSN {
		t.Fatal("with a shipped default the toggle starts on and the field holds it")
	}
	// Still nothing before the popup is answered, and the owner's "off" wins.
	if config := (Config{}).Resolve(Stored{}); config.Enabled() {
		t.Fatal("nothing is reported before the consent popup was answered")
	}
	no := false
	if config := (Config{}).Resolve(Stored{Prompted: true, Enabled: &no}); config.Enabled() {
		t.Fatal("a chosen off must win over the default")
	}
}

// Reverting the default-DSN commit must leave a working, off-by-default build: the DSN literal lives in default_dsn.go and nowhere else in
// the shipped sources, and everything else treats an empty default as "off".
func TestTheDefaultDSNLivesInOneFile(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "default_dsn.go" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "ingest.") && strings.Contains(string(data), "sentry.io") {
			t.Errorf("%s names a Sentry ingest host: the default DSN belongs in default_dsn.go only", file)
		}
	}
	saved := DefaultDSN
	DefaultDSN = "" // what the build is without that file
	defer func() { DefaultDSN = saved }()
	if config := (Config{}).Resolve(Stored{Prompted: true}); config.Enabled() || (Stored{}).EnabledOrDefault() {
		t.Fatal("without default_dsn.go the build must be off until a DSN is chosen")
	}
}
