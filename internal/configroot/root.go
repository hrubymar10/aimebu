// Package configroot resolves the shared server data root from the environment.
package configroot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FromEnv expands a leading tilde and refuses paths relative to the caller's
// working directory. The default remains the development data root.
func FromEnv() (string, error) {
	return Resolve(os.Getenv("AIMEBU2_CONFIG_DIR"))
}

// Resolve applies the same path rules to a raw config-root value.
func Resolve(raw string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if raw == "" {
		return filepath.Join(home, ".aimebu2"), nil
	}
	if raw == "~" {
		raw = home
	} else if strings.HasPrefix(raw, "~/") {
		raw = filepath.Join(home, raw[2:])
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("AIMEBU2_CONFIG_DIR must be an absolute path")
	}
	return filepath.Clean(raw), nil
}
