package switcher

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	account, err := user.Current()
	if err != nil || account.HomeDir == "" {
		fmt.Fprintln(os.Stderr, "cannot determine real home for credential test guard:", err)
		os.Exit(1)
	}
	root, err := os.MkdirTemp("/tmp", "aimebu-switcher-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create isolated credential test home:", err)
		os.Exit(1)
	}
	paths := map[string]string{
		"HOME":              root,
		"CLAUDE_CONFIG_DIR": filepath.Join(root, ".claude"),
		"CODEX_HOME":        filepath.Join(root, ".codex"),
		"AIMEBU_CONFIG_DIR": filepath.Join(root, ".aimebu"),
	}
	for key, path := range paths {
		if path == account.HomeDir || strings.HasPrefix(path, account.HomeDir+string(filepath.Separator)) {
			fmt.Fprintf(os.Stderr, "%s points under real home: %s\n", key, path)
			os.Exit(1)
		}
		if err := os.Setenv(key, path); err != nil {
			fmt.Fprintln(os.Stderr, "cannot isolate", key, err)
			os.Exit(1)
		}
	}
	if err := os.Setenv("AIMEBU_URL", "http://127.0.0.1:1"); err != nil {
		fmt.Fprintln(os.Stderr, "cannot isolate AIMEBU_URL:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, "cannot remove isolated credential test home:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
