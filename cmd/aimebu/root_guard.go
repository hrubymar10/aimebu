package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errV1RootWrite = errors.New("v1 data or an interrupted v1 move is present")

// refuseV1RootWrite protects a v1 installation until the server has moved its
// data into the cutover backup. CLI operations must not create v2 state in the
// live v1 tree, even when the caller bypasses an interactive confirmation.
func refuseV1RootWrite(root string) error {
	pending, err := v1MovePending(root)
	if err != nil {
		return err
	}
	if !pending {
		pending, err = isV1Root(root)
		if err != nil {
			return err
		}
	}
	if pending {
		return fmt.Errorf("refusing to write %s: %w; start the v2 server to finish cutover first", root, errV1RootWrite)
	}
	return nil
}

func reportSupervisorRootError(root string, err error) error {
	if errors.Is(err, errV1RootWrite) {
		return fmt.Errorf("this machine has aimebu v1 data in %s; set AIMEBU_CONFIG_DIR to a separate v2 root for the supervisor: %w", root, err)
	}
	return err
}

// oldDevelopmentRoot reports an earlier v2 install only while the caller uses
// the new default root. An empty legacy directory is not evidence of state.
func oldDevelopmentRoot() (string, bool) {
	if os.Getenv("AIMEBU_CONFIG_DIR") != "" {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	root := filepath.Join(home, ".aimebu2")
	for _, part := range []string{"server/aimebu.sqlite", "supervisor/machine.key", "supervisor/journal.sqlite", "switcher/state.json"} {
		if info, err := os.Stat(filepath.Join(root, part)); err == nil && !info.IsDir() {
			return root, true
		}
	}
	return "", false
}

func oldDevelopmentRootNotice() string {
	if root, ok := oldDevelopmentRoot(); ok {
		return fmt.Sprintf("v2 data found in %s; set AIMEBU_CONFIG_DIR=%s or move it", root, root)
	}
	return ""
}
