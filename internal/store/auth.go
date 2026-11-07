package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type User struct {
	ID         string
	Login      string
	PWHash     string
	PWParams   string
	DisabledAt sql.NullString
}

func (s *Store) UserCount(ctx context.Context) (int, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user`).Scan(&count)
	return count, err
}

func (s *Store) MachineCount(ctx context.Context) (int, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM machine`).Scan(&count)
	return count, err
}

func (s *Store) IsGlobalOwner(ctx context.Context, userID string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM user ORDER BY created_at,id LIMIT 1`).Scan(&owner)
	return owner == userID, err
}

// IsGlobalAdmin reports whether the user has the owner's reach: the owner, or an active account the owner flagged as admin.
func (s *Store) IsGlobalAdmin(ctx context.Context, userID string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var owner string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM user ORDER BY created_at,id LIMIT 1`).Scan(&owner); err != nil {
		return false, err
	}
	if owner == userID {
		return true, nil
	}
	var admin bool
	err := s.db.QueryRowContext(ctx, `SELECT admin=1 AND disabled_at IS NULL FROM user WHERE id=?`, userID).Scan(&admin)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return admin, err
}

func (s *Store) MaxPasswordIterations(ctx context.Context) (int, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return 0, err
	}
	var iterations int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(json_extract(pw_params,'$.iterations') AS INTEGER)),0) FROM user`).Scan(&iterations)
	return iterations, err
}

func (tx *Tx) CountUsers() (int, error) {
	var count int
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM user`).Scan(&count)
	return count, err
}

func (tx *Tx) CreateUser(user User) (User, error) {
	var err error
	user.ID, err = newID("usr")
	if err != nil {
		return User{}, err
	}
	_, err = tx.tx.ExecContext(tx.ctx, `INSERT INTO user(id,login,pw_hash,pw_params,created_at)
		VALUES (?,?,?,?,?)`, user.ID, user.Login, user.PWHash, user.PWParams, timestamp())
	if err != nil {
		return User{}, err
	}
	if _, err = tx.AppendEvent(Event{Kind: "user_created",
		PayloadJSON: payloadJSON(map[string]string{"user_id": user.ID})}); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Store) UserByLogin(ctx context.Context, login string) (User, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return User{}, err
	}
	var user User
	err := s.db.QueryRowContext(ctx, `SELECT id,login,pw_hash,pw_params,disabled_at FROM user WHERE login=?`, login).
		Scan(&user.ID, &user.Login, &user.PWHash, &user.PWParams, &user.DisabledAt)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return user, err
	}
	count, id, err := canonicalLoginMatches(ctx, s.db, login)
	if err != nil {
		return User{}, err
	}
	if count != 1 {
		return User{}, sql.ErrNoRows
	}
	err = s.db.QueryRowContext(ctx, `SELECT id,login,pw_hash,pw_params,disabled_at FROM user WHERE id=?`, id).
		Scan(&user.ID, &user.Login, &user.PWHash, &user.PWParams, &user.DisabledAt)
	return user, err
}

func canonicalLogin(login string) string { return strings.ToLower(strings.TrimSpace(login)) }

type loginQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Use SQLite's cheap ASCII match first. A miss may be a Unicode case fold,
// which SQLite's lower() does not implement; scan login names only then.
func canonicalLoginMatches(ctx context.Context, db loginQueryer, login string) (count int, id string, err error) {
	canonical := canonicalLogin(login)
	for _, query := range []string{
		`SELECT id,login FROM user WHERE lower(trim(login))=?`,
		`SELECT id,login FROM user`,
	} {
		var rows *sql.Rows
		if query == `SELECT id,login FROM user` {
			rows, err = db.QueryContext(ctx, query)
		} else {
			rows, err = db.QueryContext(ctx, query, canonical)
		}
		if err != nil {
			return 0, "", err
		}
		count, id = 0, ""
		for rows.Next() {
			var candidateID, candidateLogin string
			if err = rows.Scan(&candidateID, &candidateLogin); err != nil {
				rows.Close()
				return 0, "", err
			}
			if canonicalLogin(candidateLogin) == canonical {
				count++
				id = candidateID
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || count > 0 {
			return count, id, err
		}
	}
	return 0, "", nil
}

func (s *Store) UpdateUserPassword(ctx context.Context, userID, hash, params string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		result, err := tx.Exec(`UPDATE user SET pw_hash=?,pw_params=? WHERE id=?`, hash, params, userID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		_, err = tx.AppendEvent(Event{Kind: "user_password_rehashed",
			PayloadJSON: payloadJSON(map[string]string{"user_id": userID})})
		return err
	})
}

type AuthSession struct {
	ID        string
	UserID    string
	TokenHash string
	CSRFToken string
	ExpiresAt string
	RevokedAt sql.NullString
	// CreatedAt is when the session was made; its absolute lifetime counts from it, however often the expiry has slid.
	CreatedAt string
}

func (s *Store) CreateAuthSession(ctx context.Context, session AuthSession) (AuthSession, error) {
	created := session.CreatedAt
	if created == "" {
		created = timestamp()
	}
	session.CreatedAt = created
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		session.ID, err = newID("ses")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO auth_session
			(id,user_id,csrf_token,expires_at,token_hash,created_at) VALUES (?,?,?,?,?,?)`,
			session.ID, session.UserID, session.CSRFToken, session.ExpiresAt, session.TokenHash, created)
		if err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "auth_session_created",
			PayloadJSON: payloadJSON(map[string]string{"session_id": session.ID})})
		return err
	})
	return session, err
}

func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash string) (AuthSession, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AuthSession{}, err
	}
	var session AuthSession
	err := s.db.QueryRowContext(ctx, `SELECT s.id,s.user_id,s.token_hash,s.csrf_token,s.expires_at,s.revoked_at,s.created_at
		FROM auth_session s JOIN user u ON u.id=s.user_id WHERE s.token_hash=? AND u.disabled_at IS NULL`, tokenHash).Scan(&session.ID, &session.UserID,
		&session.TokenHash, &session.CSRFToken, &session.ExpiresAt, &session.RevokedAt, &session.CreatedAt)
	return session, err
}

func (s *Store) ExtendAuthSession(ctx context.Context, sessionID string, now, expiry time.Time) error {
	return s.Tx(ctx, func(tx *Tx) error {
		result, err := tx.Exec(`UPDATE auth_session SET expires_at=?
			WHERE id=? AND revoked_at IS NULL AND expires_at>?`,
			expiry.UTC().Format(timestampLayout), sessionID, now.UTC().Format(timestampLayout))
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (s *Store) RevokeAuthSession(ctx context.Context, sessionID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		result, err := tx.Exec(`UPDATE auth_session SET revoked_at=?
			WHERE id=? AND revoked_at IS NULL`, timestamp(), sessionID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		_, err = tx.AppendEvent(Event{Kind: "auth_session_revoked",
			PayloadJSON: payloadJSON(map[string]string{"session_id": sessionID})})
		return err
	})
}

type Machine struct {
	ID                string
	Name              string
	KeyHash           string
	Approved          bool
	RevokedAt         sql.NullString
	IsHost            bool
	LastSeen          sql.NullString
	SupervisorVersion sql.NullString
}

func (tx *Tx) CreateMachine(machine Machine) (Machine, error) {
	var err error
	machine.ID, err = newID("mch")
	if err != nil {
		return Machine{}, err
	}
	_, err = tx.Exec(`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES (?,?,?,?,?)`,
		machine.ID, machine.Name, machine.KeyHash, machine.Approved, machine.IsHost)
	if err != nil {
		return Machine{}, err
	}
	if _, err = tx.AppendEvent(Event{Kind: "machine_created",
		PayloadJSON: payloadJSON(map[string]string{"machine_id": machine.ID})}); err != nil {
		return Machine{}, err
	}
	return machine, nil
}

func (s *Store) MachineByID(ctx context.Context, id string) (Machine, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return Machine{}, err
	}
	var machine Machine
	err := s.db.QueryRowContext(ctx, `SELECT id,name,key_hash,approved,revoked_at,is_host,last_seen,supervisor_version FROM machine WHERE id=?`, id).
		Scan(&machine.ID, &machine.Name, &machine.KeyHash, &machine.Approved, &machine.RevokedAt, &machine.IsHost, &machine.LastSeen, &machine.SupervisorVersion)
	return machine, err
}

func (s *Store) HostMachine(ctx context.Context) (Machine, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return Machine{}, err
	}
	var m Machine
	err := s.db.QueryRowContext(ctx, `SELECT id,name,key_hash,approved,revoked_at,is_host,last_seen,supervisor_version FROM machine WHERE is_host=1`).
		Scan(&m.ID, &m.Name, &m.KeyHash, &m.Approved, &m.RevokedAt, &m.IsHost, &m.LastSeen, &m.SupervisorVersion)
	return m, err
}

func (s *Store) Machines(ctx context.Context) ([]Machine, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,key_hash,approved,revoked_at,is_host,last_seen,supervisor_version FROM machine ORDER BY is_host DESC,name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Machine{}
	for rows.Next() {
		var m Machine
		if err := rows.Scan(&m.ID, &m.Name, &m.KeyHash, &m.Approved, &m.RevokedAt, &m.IsHost, &m.LastSeen, &m.SupervisorVersion); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ApproveMachine(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		result, err := tx.Exec(`UPDATE machine SET approved=1 WHERE id=? AND approved=0 AND revoked_at IS NULL`, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return sql.ErrNoRows
		}
		_, err = tx.AppendEvent(Event{Kind: "machine_approved", PayloadJSON: payloadJSON(map[string]string{"machine_id": id})})
		return err
	})
}

func (s *Store) CreateMachinePairing(ctx context.Context, hash, name, userID, expires string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO machine_pairing(token_hash,name,created_by,expires_at) VALUES (?,?,?,?)`, hash, name, userID, expires)
		if err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "machine_pairing_created", PayloadJSON: payloadJSON(map[string]string{"name": name, "created_by": userID})})
		return err
	})
}

// RedeemMachinePairing consumes the token and creates its pending machine in
// one transaction. The caller retains the only plaintext machine key.
func (s *Store) RedeemMachinePairing(ctx context.Context, tokenHash, keyHash, now string) (Machine, error) {
	var machine Machine
	err := s.Tx(ctx, func(tx *Tx) error {
		var name string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT name FROM machine_pairing WHERE token_hash=? AND redeemed_at IS NULL AND expires_at>?`, tokenHash, now).Scan(&name)
		if err != nil {
			return err
		}
		machine, err = tx.CreateMachine(Machine{Name: name, KeyHash: keyHash})
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE machine_pairing SET redeemed_at=?,machine_id=? WHERE token_hash=?`, now, machine.ID, tokenHash)
		return err
	})
	return machine, err
}

func (s *Store) RevokeMachine(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		result, err := tx.Exec(`UPDATE machine SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, timestamp(), id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT agent_id FROM boundary_pending WHERE machine_id=? AND state='open'`, id)
		if err != nil {
			return err
		}
		var pendingAgents []string
		for rows.Next() {
			var agentID string
			if err := rows.Scan(&agentID); err != nil {
				rows.Close()
				return err
			}
			pendingAgents = append(pendingAgents, agentID)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, agentID := range pendingAgents {
			if err := tx.withdrawBoundaryPending(agentID); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(Event{Kind: "machine_revoked",
			PayloadJSON: payloadJSON(map[string]string{"machine_id": id})})
		return err
	})
}

func (s *Store) SetMachineKeyHash(ctx context.Context, id, keyHash string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		result, err := tx.Exec(`UPDATE machine SET key_hash=? WHERE id=? AND revoked_at IS NULL`, keyHash, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		_, err = tx.AppendEvent(Event{Kind: "machine_key_rotated",
			PayloadJSON: payloadJSON(map[string]string{"machine_id": id})})
		return err
	})
}

func (s *Store) AgentKeyByID(ctx context.Context, id string) (keyHash string, revoked bool, err error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", false, err
	}
	var revokedAt sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT key_hash,revoked_at FROM agent WHERE id=?`, id).
		Scan(&keyHash, &revokedAt)
	return keyHash, revokedAt.Valid, err
}

func (s *Store) SetAgentKeyHash(ctx context.Context, id, keyHash string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		result, err := tx.Exec(`UPDATE agent SET key_hash=? WHERE id=? AND revoked_at IS NULL`, keyHash, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		_, err = tx.AppendEvent(Event{Kind: "agent_key_rotated", AgentID: id,
			PayloadJSON: payloadJSON(map[string]string{"agent_id": id})})
		return err
	})
}

func (s *Store) RevokeAgent(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.revokeAgent(id, nil) })
}

// revokeAgent marks an agent revoked: its key stops working, and every query that looks for a live agent (a role's slot, a slug, a checkout's
// writer) skips it. extra adds fields to the event's payload.
func (tx *Tx) revokeAgent(id string, extra map[string]string) error {
	result, err := tx.Exec(`UPDATE agent SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, timestamp(), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	payload := map[string]string{"agent_id": id}
	for key, value := range extra {
		payload[key] = value
	}
	_, err = tx.AppendEvent(Event{Kind: "agent_revoked", AgentID: id, PayloadJSON: payloadJSON(payload)})
	return err
}
