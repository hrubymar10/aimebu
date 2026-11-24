package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The model-approval gate (A10-2): an agent-initiated spawn may not pick a
// harness or model outside the role's default pool unless the request carries
// approved_by, the message of a person in the same project that approved that
// exact choice. A person's own spawn is never gated.

// spawnChoiceInPool says whether the harness choice an agent's spawn request
// names is one of the role's default pool entries. Every field the request
// sets must match one pool entry; an empty field means the pool decides that
// part (the launcher's tool and variant, the model it lists). The agent route
// carries no launcher_id, so that part is always pool-decided there. A role
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
func (s *Server) validatedApprovalMessage(ctx context.Context, projectID, ref string) (string, error) {
	id, err := s.Store.ResolveMessageRef(ctx, ref)
	if err != nil {
		return "", err
	}
	authorKind, messageProject, err := s.Store.MessageAuthorProject(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("approved_by names no message in this project")
	}
	if err != nil {
		return "", err
	}
	if authorKind != "user" {
		return "", fmt.Errorf("approved_by is not a message written by a person")
	}
	if messageProject != projectID {
		return "", fmt.Errorf("approved_by is a message of another project")
	}
	return id, nil
}
