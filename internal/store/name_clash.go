package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrLoginTaken is a person login that an agent's slug already uses (in any project). It is the other direction of ErrSlugTaken: no name is
// shared by a login and an agent, so `@name` never has to address both.
var ErrLoginTaken = errors.New("login in use")

// NameInUseError is a name refused because something else already holds it. Holder says what, in words a person can act on. It matches
// ErrSlugTaken (an agent name) or ErrLoginTaken (a person login) with errors.Is.
type NameInUseError struct {
	Name   string
	Holder string
	kind   error
}

func (e *NameInUseError) Error() string {
	if e.kind == ErrLoginTaken {
		return fmt.Sprintf("the login %q is already held by %s", e.Name, e.Holder)
	}
	return fmt.Sprintf("the name %q is already held by %s", e.Name, e.Holder)
}

func (e *NameInUseError) Unwrap() error { return e.kind }

// loginHolder describes the person whose login equals name (case-insensitive, any project), or "" when none does.
func loginHolder(ctx context.Context, q rowQuerier, name string) (string, error) {
	var login string
	err := q.QueryRowContext(ctx, `SELECT login FROM user WHERE lower(login)=lower(?) ORDER BY created_at,id LIMIT 1`, name).Scan(&login)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return "the person with the login " + login, nil
}

// agentSlugHolder describes the agent whose slug equals name (case-insensitive) in any project, or "" when none does. A removed agent's slug
// is a tombstone, so it never matches.
func agentSlugHolder(ctx context.Context, q rowQuerier, name string) (string, error) {
	var slug, project string
	err := q.QueryRowContext(ctx, `SELECT a.slug,COALESCE(p.name,'') FROM agent a LEFT JOIN project p ON p.id=a.project_id WHERE lower(a.slug)=lower(?) AND a.revoked_at IS NULL ORDER BY a.created_at,a.id LIMIT 1`, name).Scan(&slug, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("the agent %s@%s", slug, project), nil
}

// checkLoginFree refuses a login an agent slug already holds.
func checkLoginFree(ctx context.Context, q rowQuerier, login string) error {
	holder, err := agentSlugHolder(ctx, q, strings.TrimSpace(login))
	if err != nil {
		return err
	}
	if holder != "" {
		return &NameInUseError{Name: login, Holder: holder, kind: ErrLoginTaken}
	}
	return nil
}

// slugInUse is the error for an agent name that projectID cannot have, naming what holds it: a person's login (any project), an agent or a
// role key of the project. ErrSlugTaken without a holder when nothing is found (a race that resolved meanwhile).
func slugInUse(ctx context.Context, q rowQuerier, projectID, name string) error {
	if holder, err := loginHolder(ctx, q, name); err != nil {
		return err
	} else if holder != "" {
		return &NameInUseError{Name: name, Holder: holder, kind: ErrSlugTaken}
	}
	var found string
	err := q.QueryRowContext(ctx, `SELECT slug FROM agent WHERE project_id=? AND lower(slug)=lower(?) LIMIT 1`, projectID, name).Scan(&found)
	if err == nil {
		return &NameInUseError{Name: name, Holder: "an agent of this project", kind: ErrSlugTaken}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	err = q.QueryRowContext(ctx, `SELECT role_key FROM role WHERE deleted_at IS NULL AND (project_id IS NULL OR project_id=?) AND lower(role_key)=lower(?) LIMIT 1`, projectID, name).Scan(&found)
	if err == nil {
		return &NameInUseError{Name: name, Holder: "a role key", kind: ErrSlugTaken}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return ErrSlugTaken
}

// SlugInUse is nil when projectID can name an agent name, or the error naming what holds it.
func (s *Store) SlugInUse(ctx context.Context, projectID, name string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	taken, err := takenSlugs(ctx, s.db, projectID)
	if err != nil {
		return err
	}
	if !taken[strings.ToLower(name)] {
		return nil
	}
	return slugInUse(ctx, s.db, projectID, name)
}
