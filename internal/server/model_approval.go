package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The model-approval gate (A10-2): an agent-initiated spawn may not pick a
// harness or model outside the role's default pool unless the request carries
// approved_by, the message of a person in the same project that approved that
// exact choice. A person's own spawn is never gated.

// spawnChoiceInPool says whether the harness choice an agent's spawn request
// names is one of the role's default pool entries. Every field the request
// sets must match one pool entry; an empty field means the pool decides that
// part (the launcher's tool and variant, the model it lists). A launcher_id
// the request names must be the pool entry's launcher. A role
// with no configured default has no pool to be outside of: nothing is
// pre-approved, and the existing rule for that case (an unavailable default
// needs a reason and says so in the room notice) stands. A pool entry's model
// is never empty (ValidateSpawnDefaults), so a request that names a model
// matches only an entry configured with that model.
func (s *Server) spawnChoiceInPool(ctx context.Context, projectID, roleKey string, req agentSpawnRequest) (bool, error) {
	defaults, err := s.Store.RoleSpawnDefaults(ctx, projectID, roleKey)
	if err != nil {
		return false, err
	}
	if len(defaults.DefaultAgents) == 0 {
		return true, nil
	}
	for _, choice := range defaults.DefaultAgents {
		launcher, err := s.Store.SpawnDefaultLauncher(ctx, choice.LauncherID)
		if errors.Is(err, sql.ErrNoRows) {
			continue // a launcher that no longer exists approves nothing
		}
		if err != nil {
			return false, err
		}
		if req.LauncherID != "" && req.LauncherID != choice.LauncherID {
			continue
		}
		if req.Tool != "" && req.Tool != launcher.Tool {
			continue
		}
		if req.Variant != "" && req.Variant != launcherAlias(launcher.Name) && req.Variant != launcher.Name {
			continue
		}
		if req.Model != "" && req.Model != choice.Model {
			continue
		}
		return true, nil
	}
	return false, nil
}

// validatedApprovalMessage resolves an approved_by reference and returns the
// message id when it names a message a person wrote in a room of this
// project. Anything else is refused with the reason named.
func (s *Server) validatedApprovalMessage(ctx context.Context, projectID, agentID, ref string, req agentSpawnRequest, outOfPool bool) (string, error) {
	id, err := s.Store.ResolveMessageRef(ctx, ref)
	if err != nil {
		return "", err
	}
	message, messageProject, readable, err := s.Store.SpawnApprovalMessage(ctx, id, agentID, projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("approved_by names no message in this project")
	}
	if err != nil {
		return "", err
	}
	if message.AuthorKind != "user" {
		return "", fmt.Errorf("approved_by is not a message written by a person")
	}
	if messageProject != projectID {
		return "", fmt.Errorf("approved_by is a message of another project")
	}
	if !outOfPool {
		return id, nil
	}
	if !readable {
		return "", fmt.Errorf("approved_by is in a room the requesting agent cannot read")
	}
	created, err := time.Parse(time.RFC3339Nano, message.CreatedAt)
	now := time.Now().UTC()
	if err != nil || created.Before(now.Add(-7*24*time.Hour)) || created.After(now) {
		return "", fmt.Errorf("approved_by is older than 7 days or has an invalid date")
	}
	if req.Model == "" {
		return "", fmt.Errorf("approved_by requires an explicit requested model")
	}
	labels, err := s.Store.SpawnModelLabels(ctx, req.Tool, req.Variant, req.Model)
	if err != nil {
		return "", err
	}
	modelNamed := approvalNamesChoice(message.Body, req.Model)
	for _, label := range labels {
		modelNamed = modelNamed || approvalNamesChoice(message.Body, label)
	}
	if !modelNamed {
		return "", fmt.Errorf("approved_by does not name the requested model")
	}
	harnessNames, err := s.Store.SpawnApprovalHarnessNames(ctx)
	if err != nil {
		return "", err
	}
	for _, name := range harnessNames {
		if approvalNamesChoice(message.Body, name.Tool) && !strings.EqualFold(name.Tool, req.Tool) {
			return "", fmt.Errorf("approved_by names a different tool or variant")
		}
		if (approvalNamesChoice(message.Body, name.Variant) || approvalNamesChoice(message.Body, launcherAlias(name.Variant))) &&
			!strings.EqualFold(req.Variant, name.Variant) && !strings.EqualFold(req.Variant, launcherAlias(name.Variant)) {
			return "", fmt.Errorf("approved_by names a different tool or variant")
		}
	}
	return id, nil
}

func approvalNamesChoice(body, choice string) bool {
	if choice == "" {
		return false
	}
	body, choice = strings.ToLower(body), strings.ToLower(choice)
	for start := 0; start < len(body); {
		at := strings.Index(body[start:], choice)
		if at < 0 {
			return false
		}
		at += start
		end := at + len(choice)
		before, _ := utf8.DecodeLastRuneInString(body[:at])
		after, _ := utf8.DecodeRuneInString(body[end:])
		beforeOK := at == 0 || !approvalWordRune(before)
		afterOK := end == len(body) || !approvalWordRune(after)
		if beforeOK && afterOK {
			return true
		}
		start = at + 1
	}
	return false
}

func approvalWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_./:@+-", r)
}
