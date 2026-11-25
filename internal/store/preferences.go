package store

import "context"

// UserPreferences returns every stored interface preference of a user, by name.
func (s *Store) UserPreferences(ctx context.Context, userID string) (map[string]string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM user_preference WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

// SetUserPreferences stores the given preferences of a user in one transaction; an existing value is replaced.
func (s *Store) SetUserPreferences(ctx context.Context, userID string, prefs map[string]string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		now := timestamp()
		for key, value := range prefs {
			if _, err := tx.Exec(`INSERT INTO user_preference(user_id,key,value,updated_at) VALUES (?,?,?,?)
				ON CONFLICT(user_id,key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, userID, key, value, now); err != nil {
				return err
			}
		}
		return nil
	})
}
