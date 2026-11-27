package live

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitCommand keeps scratch repositories independent of the host's Git settings.
// The identity is only for commits made while building scenario fixtures.
func gitCommand(dir string, args ...string) *exec.Cmd {
	full := append([]string{"-C", dir, "-c", "user.name=eval", "-c", "user.email=eval@example.invalid", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	return cmd
}

// ensureScratchExclude keeps the harness's private state out of plain git add -A.
func ensureScratchExclude(dir string) error {
	out, err := gitCommand(dir, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return fmt.Errorf("locating scratch Git exclude: %w", err)
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == ".aimebu/" {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := file.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = file.WriteString(".aimebu/\n")
	return err
}
