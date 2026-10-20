package store

import "context"

// RecordCredentialAlert records an operator-facing credential event (a login
// was imported, a refresh needs a human, a token is about to expire) as a
// project-less system event, which only the owner sees. detail never contains
// credential material: the credential pieces only pass profile names and file
// basenames, and the server's alert sink redacts anything token-shaped in the
// text before it reaches here.
func (s *Store) RecordCredentialAlert(ctx context.Context, kind, detail string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "credential_alert",
			PayloadJSON: payloadJSON(map[string]string{"kind": kind, "detail": detail})})
		return err
	})
}
