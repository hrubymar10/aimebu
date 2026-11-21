package switcher

import (
	"os"
	"path/filepath"

	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

// TokenCopyConflicts finds another default switcher root that claims the same
// migrated tool and resolves its live credential to the same file. It is a
// startup guard against two independent refresh journals spending one token.
func (m *Manager) TokenCopyConflicts() (map[string]string, error) {
	conflicts := map[string]string{}
	home, err := m.ResolveHome()
	if err != nil {
		return nil, err
	}
	current, ok := usages.ReadTokenCopiesMarker(m.root)
	if !ok {
		return conflicts, nil
	}
	currentRoot, err := canonicalTokenPath(m.root)
	if err != nil {
		return nil, err
	}
	for _, candidate := range []string{
		filepath.Join(home, ".aimebu", "switcher"),
		filepath.Join(home, ".aimebu2", "switcher"),
	} {
		otherRoot, err := canonicalTokenPath(candidate)
		if err != nil {
			return nil, err
		}
		if otherRoot == currentRoot {
			continue
		}
		otherMarker, ok := usages.ReadTokenCopiesMarker(candidate)
		if !ok {
			continue
		}
		other, err := NewReadOnly(candidate)
		if err != nil {
			return nil, err
		}
		if m.homeDir != "" {
			other.SetHomeDir(m.homeDir)
		}
		for _, tool := range Tools() {
			if _, migrated := current.Since(tool); !migrated {
				continue
			}
			if _, migrated := otherMarker.Since(tool); !migrated {
				continue
			}
			live, err := m.LiveCredPath(tool)
			if err != nil {
				return nil, err
			}
			otherLive, err := other.LiveCredPath(tool)
			if err != nil {
				return nil, err
			}
			live, err = canonicalTokenPath(live)
			if err != nil {
				return nil, err
			}
			otherLive, err = canonicalTokenPath(otherLive)
			if err != nil {
				return nil, err
			}
			if live == otherLive {
				conflicts[tool] = candidate
			}
		}
	}
	return conflicts, nil
}

func canonicalTokenPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if os.IsNotExist(err) {
		return filepath.Clean(absolute), nil
	}
	return "", err
}
