package ccsessions

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Copy puts a copy of a session under the project directory Claude uses for toCWD, so that `claude --resume <id>` started in toCWD finds
// it (Claude looks a session up by the folder it runs in). The original is left as it is. The transcript and, when there is one, the
// session's own directory (tool results, subagent transcripts) are copied; an existing copy is not overwritten, so a copy that was made
// before is reused once it is checked (see verifyCopy). It returns the directory the copy sits in. Only the session id and the two folders decide
// the paths, and the id is validated, so nothing outside <config dir>/projects is touched. Every directory and file it writes is made or
// opened without following a link, and a link where one is not expected is refused, so another process that can write in the projects folder
// cannot point the copy at something else.
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
	if err := makeRealDir(to); err != nil {
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

// makeRealDir makes the directory (and its parents, as MkdirAll does) and insists the last one is a real directory, not a link to one, and
// is the person's own.
func makeRealDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory, not a link", path)
	}
	if !ownedByUs(info) {
		return fmt.Errorf("%s belongs to another user", path)
	}
	return nil
}

func ownedByUs(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(stat.Uid) == os.Getuid()
}

func copyFile(from, to string) error {
	if info, err := os.Lstat(to); err == nil {
		return verifyCopy(from, to, info) // a copy made before stays, once it is checked
	}
	in, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if info, err := in.Stat(); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", from)
	}
	// A leftover of an interrupted copy is removed first (never followed), and the new file is made exclusively without following a link, so
	// nothing planted at that name can receive the transcript.
	partial := to + ".partial"
	if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
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

// verifyCopy checks a copy made before is what it should be before it is reused: a regular file of the person's own (not a link, not
// something another user left), that agrees with the original as far as both go. Claude appends to the copy it resumes and the original may
// carry on too, so one may be longer; a copy that disagrees with the original within the shorter one is not a copy of it.
func verifyCopy(from, to string, info os.FileInfo) error {
	if !info.Mode().IsRegular() || !ownedByUs(info) {
		return fmt.Errorf("%s is not a regular file of yours; remove it so the session can be copied again", to)
	}
	a, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := os.OpenFile(to, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer b.Close()
	ra, rb := bufio.NewReader(a), bufio.NewReader(b)
	bufA, bufB := make([]byte, 32<<10), make([]byte, 32<<10)
	for {
		n, errA := io.ReadFull(ra, bufA)
		m, errB := io.ReadFull(rb, bufB)
		k := min(n, m)
		if string(bufA[:k]) != string(bufB[:k]) {
			return fmt.Errorf("%s does not match the session it was copied from; remove it so the session can be copied again", to)
		}
		if n < len(bufA) || m < len(bufB) || errA != nil || errB != nil {
			return nil // the shorter of the two ended, and everything up to there agreed
		}
	}
}

// copyTree copies the regular files and directories under from, skipping anything else (links are not followed, and a link where a directory
// of the copy should be is refused).
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
			return makeRealDir(target)
		case entry.Type().IsRegular():
			return copyFile(path, target)
		}
		return nil
	})
}
