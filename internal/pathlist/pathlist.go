package pathlist

import (
	"os"
	"path/filepath"
	"strings"
)

// Directories lists at most 200 immediate directory names. Stat follows links,
// so a link is offered only when its target is a directory.
func Directories(path, prefix string) []string {
	if !filepath.IsAbs(path) {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	names := make([]string, 0, min(len(entries), 200))
	for _, entry := range entries {
		if len(names) == 200 {
			break
		}
		if !strings.HasPrefix(entry.Name(), prefix) || strings.HasPrefix(entry.Name(), ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if entry.IsDir() {
			names = append(names, entry.Name())
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(path, entry.Name())); err == nil && info.IsDir() {
				names = append(names, entry.Name())
			}
		}
	}
	return names
}
