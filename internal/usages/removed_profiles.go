package usages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const maxRemovedProfiles = 50

var errCorruptRemovedProfiles = errors.New("corrupt removed.json")

type removedProfile struct {
	Tool      string    `json:"tool"`
	Name      string    `json:"name"`
	TokenHash string    `json:"token_sha256"`
	RemovedAt time.Time `json:"removed_at"`
}

type removedProfilesFile struct {
	Entries []removedProfile `json:"entries"`
}

func removedProfilesPath(root string) string { return filepath.Join(root, "removed.json") }

func readRemovedProfiles(root string) (removedProfilesFile, error) {
	data, err := os.ReadFile(removedProfilesPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return removedProfilesFile{}, nil
	}
	if err != nil {
		return removedProfilesFile{}, err
	}
	var file removedProfilesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return removedProfilesFile{}, fmt.Errorf("%w: %v", errCorruptRemovedProfiles, err)
	}
	return file, nil
}

func refreshTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RecordRemovedProfile stores only a hash of the removed refresh token. The
// caller holds the switcher lock and calls this before deleting the profile.
func RecordRemovedProfile(root, tool, name string, credentials []byte) error {
	token, err := refreshToken(credentials, tool)
	if err != nil {
		// Broken or API-key-only credentials must still be removable. There is
		// no refresh token to identify an in-flight rotation in this case.
		log.Printf("switcher: %s/%s: skip removal tombstone: %v", tool, name, err)
		return nil
	}
	file, err := readRemovedProfiles(root)
	if err != nil {
		if !errors.Is(err, errCorruptRemovedProfiles) {
			return err
		}
		log.Printf("ERROR switcher: %s/%s: read removed.json: %v; replacing corrupt tombstones", tool, name, err)
		file = removedProfilesFile{}
	}
	file.Entries = append(file.Entries, removedProfile{
		Tool: tool, Name: name, TokenHash: refreshTokenHash(token), RemovedAt: time.Now().UTC(),
	})
	if len(file.Entries) > maxRemovedProfiles {
		file.Entries = file.Entries[len(file.Entries)-maxRemovedProfiles:]
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteBytes(removedProfilesPath(root), append(data, '\n'), 0o600)
}

// WasRefreshTokenRemoved checks an in-flight rotation against explicit Remove
// operations. A read error must abort copy-back and rescue the rotated bytes.
func WasRefreshTokenRemoved(root, tool, token string) (bool, error) {
	_, found, err := removedTokenTime(root, tool, token)
	return found, err
}

func removedTokenTime(root, tool, token string) (time.Time, bool, error) {
	file, err := readRemovedProfiles(root)
	if err != nil {
		if !errors.Is(err, errCorruptRemovedProfiles) {
			return time.Time{}, false, err
		}
		log.Printf("ERROR usages: read removed.json: %v; treating tombstones as empty", err)
		return time.Time{}, false, nil
	}
	hash := refreshTokenHash(token)
	var latest time.Time
	var found bool
	for _, entry := range file.Entries {
		if entry.Tool == tool && entry.TokenHash == hash {
			found = true
			if entry.RemovedAt.After(latest) {
				latest = entry.RemovedAt
			}
		}
	}
	return latest, found, nil
}
