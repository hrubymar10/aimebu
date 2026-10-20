package usages

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "aimebu-v2-usages-tests-")
	if err != nil {
		panic(err)
	}
	for name, value := range map[string]string{
		"HOME": root,
		// An empty PATH keeps the Claude user-agent probe from running a real claude.
		"PATH":               root,
		"CODEX_HOME":         filepath.Join(root, ".codex"),
		"CLAUDE_CONFIG_DIR":  filepath.Join(root, ".claude"),
		"AIMEBU2_CONFIG_DIR": filepath.Join(root, ".aimebu2"),
	} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
