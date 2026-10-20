package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"sort"

	"github.com/goccy/go-json"
)

// A v1 person can be mapped to an existing v2 account, skipped, or given a new account made by the import (V1ImportMap.NewUsers).
// The new account has a login the owner chose and a password nobody knows, so it cannot sign in until its owner follows a one-time
// reset link the server creates once the import has committed (see the server's import job). Its messages are attributed to it, and
// it is a member of every project one of its rooms lands in, at the role the owner chose.

// V1NewUser is the account to make for one v1 person.
type V1NewUser struct {
	Login string `json:"login"`
	Role  string `json:"role"` // "viewer" or "operator"
}

// V1NewAccount is an account an import made (or found already made by an earlier run of the same import).
type V1NewAccount struct {
	V1Name string `json:"v1_name"`
	Login  string `json:"login"`
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// validV1Login mirrors the login rule of invitations (3 to 32 characters: a lowercase letter, then lowercase letters, digits, - or _).
func validV1Login(login string) bool {
	if len(login) < 3 || len(login) > 32 || login[0] < 'a' || login[0] > 'z' {
		return false
	}
	for _, ch := range login[1:] {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

// validateV1NewUsers checks the new accounts against the mapping before anything is written.
func validateV1NewUsers(data *v1ImportData, mapping V1ImportMap) error {
	seen := map[string]string{}
	for _, name := range sortedImportKeys(mapping.NewUsers) {
		user := mapping.NewUsers[name]
		if v1ImportKind(name, data.Agents) != "user" {
			return fmt.Errorf("new account for %q: only a person can get an account", name)
		}
		if !validV1Login(user.Login) {
			return fmt.Errorf("new account for %q: the login must be 3 to 32 characters, a lowercase letter then lowercase letters, digits, - or _", name)
		}
		if user.Role != "viewer" && user.Role != "operator" {
			return fmt.Errorf("new account for %q: the role must be viewer or operator", name)
		}
		if mapping.Humans[name] != user.Login {
			return fmt.Errorf("new account for %q: that person must be mapped to the new login %q", name, user.Login)
		}
		if other, dup := seen[user.Login]; dup {
			return fmt.Errorf("new accounts for %q and %q would both be %q", other, name, user.Login)
		}
		seen[user.Login] = name
	}
	return nil
}

// importV1NewUsers makes the new accounts (the ledger remembers them, so a rerun finds them) and says which projects each belongs to,
// as grants to apply while the projects are made. An account's projects are those where it was a member or an author of a room that
// is imported.
func importV1NewUsers(ctx context.Context, tx *sql.Tx, data *v1ImportData, mapping V1ImportMap, report *V1ImportReport) (map[string][]v1Grant, error) {
	grants := map[string][]v1Grant{}
	for _, name := range sortedImportKeys(mapping.NewUsers) {
		user := mapping.NewUsers[name]
		id, found, err := importMapID(ctx, tx, mapping.SourceKey, "user", name)
		if err != nil {
			return nil, err
		}
		if found {
			report.AlreadyPresent["accounts"]++
		} else {
			count, _, err := canonicalLoginMatches(ctx, tx, user.Login)
			if err != nil {
				return nil, err
			}
			if count > 0 {
				return nil, fmt.Errorf("new account for %q: the login %q is already taken", name, user.Login)
			}
			hash, params, err := unusablePassword()
			if err != nil {
				return nil, err
			}
			if id, err = newID("usr"); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?,?,?,?)`, id, user.Login, hash, params, timestamp()); err != nil {
				return nil, err
			}
			if err := addImportMap(ctx, tx, mapping.SourceKey, "user", name, id); err != nil {
				return nil, err
			}
			report.Imported["accounts"]++
		}
		report.NewAccounts = append(report.NewAccounts, V1NewAccount{V1Name: name, Login: user.Login, UserID: id, Role: user.Role})
		withDMs := v1PersonProjects(data, mapping, name, true)
		withoutDMs := map[string]bool{}
		for _, alias := range v1PersonProjects(data, mapping, name, false) {
			withoutDMs[alias] = true
		}
		for _, alias := range withDMs {
			grants[alias] = append(grants[alias], v1Grant{UserID: id, Role: user.Role})
			// Said out loud, because nothing else in the backup makes this person part of the project: a private chat is enough.
			if !withoutDMs[alias] {
				report.DMOnlyAccess = append(report.DMOnlyAccess, fmt.Sprintf("%s gets %s access to project %s through a direct message", user.Login, user.Role, projectLabel(mapping, alias)))
			}
		}
	}
	return grants, nil
}

type v1Grant struct{ UserID, Role string }

// projectLabel is the name the owner gave a project in the mapping, or its v1 alias.
func projectLabel(mapping V1ImportMap, alias string) string {
	if name := mapping.Projects[alias].Name; name != "" {
		return name
	}
	return alias
}

// v1PersonProjects lists the projects a v1 person's imported rooms go into: rooms they were a member of or wrote in. Direct messages
// count only when includeDMs is set.
func v1PersonProjects(data *v1ImportData, mapping V1ImportMap, name string, includeDMs bool) []string {
	byRoom := map[string]bool{}
	for _, room := range data.Rooms {
		for _, member := range room.Members {
			if member == name {
				byRoom[room.ID] = true
			}
		}
	}
	for _, message := range data.Messages {
		if message.From == name {
			byRoom[message.RoomID] = true
		}
	}
	projects := map[string]bool{}
	for roomID := range byRoom {
		spec := mapping.Rooms[roomID]
		alias := spec.Project
		if alias == "" {
			alias = mapping.DefaultProject
		}
		if spec.Skip || alias == "" || !includeDMs && isV1DM(roomID) {
			continue
		}
		projects[alias] = true
	}
	out := make([]string, 0, len(projects))
	for alias := range projects {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

// unusablePassword is a well-formed password hash for a random secret nobody holds, in the format the sign-in code verifies (see
// internal/auth: PBKDF2-HMAC-SHA256, the salt and parameters stored beside the hash). Signing in with it always fails; a reset link
// replaces it.
func unusablePassword() (hash, params string, err error) {
	salt := make([]byte, 16)
	secret := make([]byte, 32)
	if _, err = rand.Read(salt); err != nil {
		return "", "", err
	}
	if _, err = rand.Read(secret); err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(map[string]any{"algo": "pbkdf2-hmac-sha256", "iterations": 600000, "salt_length": len(salt), "salt": base64.RawStdEncoding.EncodeToString(salt)})
	if err != nil {
		return "", "", err
	}
	return base64.RawStdEncoding.EncodeToString(secret), string(encoded), nil
}
