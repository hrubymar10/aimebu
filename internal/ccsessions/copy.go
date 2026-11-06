package ccsessions

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Copy puts a copy of a session under the project directory Claude uses for toCWD, so that `claude --resume <id>` started in toCWD finds
// it (Claude looks a session up by the folder it runs in). The original is left as it is. The transcript and, when there is one, the
// session's own directory (tool results, subagent transcripts) are copied; an existing copy is not overwritten, so a copy that was made
// before is reused. It returns the directory the copy sits in. Only the session id and the two folders decide the paths, and the id is
// validated, so nothing outside <config dir>/projects is touched.
func Copy(configDir, id, toCWD string) (string, error) {
	if !filepath.IsAbs(toCWD) {
		return "", errors.New("the folder to copy the session to must be an absolute path")
	}
	session, err := Find(configDir, id)
	if err != nil {
		return "", err
	}
	projects := filepath.Join(configDir, "projects")
	from := filepath.Join(projects, session.Dir)
	to := filepath.Join(projects, FolderName(toCWD))
	if from == to {
		return to, nil
	}
	if err := os.MkdirAll(to, 0o700); err != nil {
		return "", err
	}
	if err := copyFile(filepath.Join(from, id+".jsonl"), filepath.Join(to, id+".jsonl")); err != nil {
		return "", err
	}
	if info, err := os.Lstat(filepath.Join(from, id)); err == nil && info.IsDir() {
		if err := copyTree(filepath.Join(from, id), filepath.Join(to, id)); err != nil {
			return "", err
		}
	}
	return to, nil
}

func copyFile(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return nil // a copy made before stays
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if info, err := in.Stat(); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", from)
	}
	out, err := os.OpenFile(to+".partial", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(out.Name())
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(out.Name())
		return err
	}
	return os.Rename(out.Name(), to)
}

// copyTree copies the regular files and directories under from, skipping anything else (links are not followed).
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o700)
		case entry.Type().IsRegular():
			return copyFile(path, target)
		}
		return nil
	})
}
