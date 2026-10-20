package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// LauncherArgs adds owner-configured argv before the adapter's protocol flags.
// No shell parses either set of arguments.
func LauncherArgs(spec TurnSpec, protocolArgs []string) []string {
	return append(append([]string(nil), spec.LauncherArgs...), protocolArgs...)
}

// LauncherEnv resolves references only when starting the process. Turn specs
// journal variable names, never the referenced values.
func LauncherEnv(env []string, refs map[string]string) []string {
	for target, source := range refs {
		env = append(env, target+"="+os.Getenv(source))
	}
	return env
}

// VisiblePath translates a file beneath the host checkout for a launcher
// whose process sees that checkout at a different absolute path.
func VisiblePath(spec TurnSpec, hostPath string) (string, error) {
	if spec.VisibleCheckoutPath == "" {
		return hostPath, nil
	}
	rel, err := filepath.Rel(spec.CheckoutPath, hostPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("launcher file is outside checkout")
	}
	return filepath.Join(spec.VisibleCheckoutPath, rel), nil
}
