package config

import (
	"errors"
	"log"
	"os"
	"path/filepath"
)

// MigrateServer moves legacy root-level server state into <root>/server.
func MigrateServer(root string) error {
	return migrateEntries(root, filepath.Join(root, "server"), serverLegacyEntries)
}

// MigrateAgents moves legacy root-level agent state into <root>/agents.
func MigrateAgents(root string) error {
	return migrateEntries(root, filepath.Join(root, "agents"), agentLegacyEntries)
}

func migrateEntries(root, targetDir string, names []string) error {
	var moved, skipped []string
	for _, name := range names {
		src := filepath.Join(root, name)
		dst := filepath.Join(targetDir, name)

		if _, err := os.Stat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if _, err := os.Stat(dst); err == nil {
			skipped = append(skipped, name)
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		moved = append(moved, name)
	}

	if len(moved) > 0 {
		log.Printf("config migration: moved %v into %s", moved, targetDir)
	}
	for _, name := range skipped {
		log.Printf("config migration: legacy %s still present at root; ignoring because %s already exists", name, filepath.Join(targetDir, name))
	}

	return nil
}
