package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

const credentialTargetsFile = "credential-targets.json"

// carryV1CredentialTargets restores the external target registry after the
// v1 server directory has moved into the backup. It never overwrites a v2
// registry, so a restarted cutover is safe to repeat.
func carryV1CredentialTargets(root string) (bool, error) {
	source := filepath.Join(root, v1BackupName, "server", credentialTargetsFile)
	targetDir := filepath.Join(root, "server")
	target := filepath.Join(targetDir, credentialTargetsFile)
	if _, err := os.Lstat(target); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("v1 credential target registry is not a regular file: %s", source)
	}
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return false, err
	}
	input, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer input.Close()
	tmp, err := os.CreateTemp(targetDir, ".credential-targets-*.tmp")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := io.Copy(tmp, input); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	// A hard link publishes the fully synced file atomically without replacing
	// a registry another process may have created after the first check.
	if err := os.Link(tmp.Name(), target); err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := syncDirectory(targetDir); err != nil {
		return false, err
	}
	log.Printf("v1 cutover: carried over %s", filepath.Join("server", credentialTargetsFile))
	return true, nil
}
