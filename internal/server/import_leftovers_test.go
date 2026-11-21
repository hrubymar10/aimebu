package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// An imported message is a plain native message. Nothing in the UI, the message API or the message and
// attachment and memory tables may remember that a row came from v1: no raw payload column, no "imported v1"
// details block, no renderer that branches on one, and no pending-review state or endpoint for imported memory.
func TestNoImportedMessageLeftoversInTheUIOrTheAPI(t *testing.T) {
	t.Parallel()
	banned := regexp.MustCompile(`legacy_payload|legacy_author_label|legacy-details|legacyDetails|Imported v1 details|LegacyPayloadJSON|LegacyAuthorLabel|legacy_source_message_id|LegacySourceMessageID|imported-memory|Review imported|ReviewImportedMemory|\.Imported\b|item\.imported`)
	// Documentation must not describe the removed columns either.
	banned = regexp.MustCompile(banned.String() + `|legacy_metadata_json`)
	for _, root := range []string{"../../web", "../store", ".", "../../docs", "../../README.md"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || strings.HasSuffix(path, "_test.go") || !regexp.MustCompile(`\.(go|js|css|html|md)$`).MatchString(path) {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(data), "\n") {
				// Migrations are history; report.Imported is the importer's own counter map.
				if banned.MatchString(line) && !strings.Contains(path, "migrations.go") && !strings.Contains(line, "report.Imported") &&
					!strings.Contains(line, "dry.Imported") && !strings.Contains(line, "Report.Imported") {
					t.Errorf("%s still mentions a removed imported-message field: %s", path, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	view := messageView(store.Message{ID: "7", Seq: 7, RoomID: "room", AuthorKind: "agent", AuthorID: "bot@repo", AuthorHarness: "codex", AuthorModel: "gpt-6-sol", SwitchHandoffJSON: `{"agent_slug":"bot","source":"written"}`, Body: "plain"})
	for key := range view {
		if strings.Contains(key, "legacy") || strings.Contains(key, "imported") {
			t.Errorf("the message view exposes %q", key)
		}
	}
	if view["author_harness"] != "codex" || view["author_model"] != "gpt-6-sol" {
		t.Errorf("harness and model are native message fields: %+v", view)
	}
	if string(view["switch_handoff"].(json.RawMessage)) != `{"agent_slug":"bot","source":"written"}` {
		t.Errorf("switch handoff metadata missing: %+v", view)
	}
}
