package agenttools

import (
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/hrubymar10/aimebu/v2/internal/runtimeignore"
)

// MaxAttachmentBytes is the largest attachment attachment_get returns or saves. It matches the upload limit (store.MaxAttachmentSize); raise
// both together.
const MaxAttachmentBytes = 10 << 20

// maxInlineText is the largest text attachment attachment_get returns inline.
const maxInlineText = 64 << 10

// maxSavedNameBytes bounds a saved file's name, well under the 255-byte limit of common file systems.
const maxSavedNameBytes = 200

// WorkdirEnv names the variable that carries the agent's workspace to the stdio bridge, so attachment_get can save a file the agent can open.
const WorkdirEnv = "AIMEBU_AGENT_WORKDIR"

var savedAttachmentID = regexp.MustCompile(`^att_[A-Za-z0-9_-]{1,96}$`)

// sanitizeAttachmentName turns a stored display name into one safe file name: the last path element only, no control or format characters,
// no leading dots, valid UTF-8, at most maxSavedNameBytes bytes (keeping a short extension), and "attachment" when nothing is left.
func sanitizeAttachmentName(name string) string {
	name = strings.ToValidUTF8(name, "_")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	name = strings.TrimRight(name, ". ")
	if len(name) > maxSavedNameBytes {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		stem := name[:len(name)-len(ext)]
		cut := maxSavedNameBytes - len(ext)
		for cut > 0 && !utf8.RuneStart(stem[cut]) {
			cut--
		}
		name = strings.TrimRight(stem[:cut], ". ") + ext
	}
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return "attachment"
	}
	return name
}

// AttachmentFileName reads the original name from a download's Content-Disposition; the server always sets one for a file it does not show
// inline, and an inline image has none.
func AttachmentFileName(contentDisposition string) string {
	_, params, err := mime.ParseMediaType(contentDisposition)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// SaveAttachment writes data to <workdir>/.aimebu/attachments/<id>/<name> with mode 0600 and returns the absolute path. It never writes
// outside the workspace: every directory on the way is created by it or must already be a real directory (a link is refused), and the final
// path is checked against the resolved workspace. The file is written under a temporary name and renamed into place, so a partial write is
// never seen under the final name and an earlier copy, or a link planted at that name, is replaced rather than followed.
func SaveAttachment(workdir, id, name string, data []byte) (string, error) {
	return saveAttachment(workdir, id, name, data, false)
}

// SaveAttachmentLikeCheckout is SaveAttachment for a file another user may have to read: the agent of a container whose user is not the one
// this process runs as. The folders it makes under .aimebu/attachments and the file take the checkout's owner and group (when this process
// may give them away) and the checkout's own read access for its group and others, never more: a checkout the container can read gives a
// file it can read, and a private checkout keeps the file 0600. .aimebu/ itself only gains the checkout's search bits (never read), so the
// folder can be passed through but not listed. The supervisor uses it for an agent on the HTTP toolbox.
func SaveAttachmentLikeCheckout(workdir, id, name string, data []byte) (string, error) {
	return saveAttachment(workdir, id, name, data, true)
}

// checkoutOwner and chownPath are swapped by tests, which cannot own files as another user.
var checkoutOwner = func(info os.FileInfo) (uid, gid int, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}
var chownPath = os.Lchown

// likeCheckout gives path the checkout's owner and the given mode. A refused change of owner (this process may not give a file away) is not
// an error: the mode still carries the checkout's own read access.
func likeCheckout(path string, mode os.FileMode, uid, gid int, chown bool) error {
	if chown {
		if err := chownPath(path, uid, gid); err != nil && !errors.Is(err, os.ErrPermission) && !errors.Is(err, syscall.EPERM) {
			return err
		}
	}
	return os.Chmod(path, mode)
}

func saveAttachment(workdir, id, name string, data []byte, like bool) (string, error) {
	if !savedAttachmentID.MatchString(id) {
		return "", errors.New("invalid attachment_id")
	}
	if workdir == "" || !filepath.IsAbs(workdir) {
		return "", errors.New("no workspace is known to save into")
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(workdir))
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return "", errors.New("workspace is not a directory")
	}
	dirMode, fileMode := os.FileMode(0700), os.FileMode(0600)
	uid, gid, chown := 0, 0, false
	if like {
		dirMode |= rootInfo.Mode().Perm() & 0055
		fileMode |= rootInfo.Mode().Perm() & 0044
		var known bool
		uid, gid, known = checkoutOwner(rootInfo)
		chown = known && (uid != os.Geteuid() || gid != os.Getegid())
	}
	// .aimebu/ with its "*" .gitignore, so a saved file can never be staged.
	if err := runtimeignore.Ensure(root); err != nil {
		return "", err
	}
	dir := root
	for _, part := range []string{".aimebu", "attachments", id} {
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s must be a real directory, not a link", dir)
		}
		switch {
		case like && part == ".aimebu":
			// The agent's runtime folder keeps its owner and stays unlistable; it only gains the checkout's search access, so a container
			// user other than this process's can reach attachments/ by name.
			if search := rootInfo.Mode().Perm() & 0011; info.Mode().Perm()&search != search {
				if err := os.Chmod(dir, info.Mode().Perm()|search); err != nil {
					return "", err
				}
			}
		case like:
			if err := likeCheckout(dir, dirMode, uid, gid, chown); err != nil {
				return "", err
			}
		}
	}
	if resolved, err := filepath.EvalSymlinks(dir); err != nil || resolved != dir {
		return "", errors.New("attachment directory resolves outside the workspace")
	}
	final := filepath.Join(dir, sanitizeAttachmentName(name))
	if rel, err := filepath.Rel(root, final); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Dir(final) != dir {
		return "", errors.New("attachment path leaves the workspace")
	}
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return "", err
	}
	if like {
		if err := likeCheckout(tmp.Name(), fileMode, uid, gid, chown); err != nil {
			tmp.Close()
			return "", err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}
