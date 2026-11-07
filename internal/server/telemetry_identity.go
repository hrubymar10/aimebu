package server

import (
	"context"
	"net/http"

	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

// Each person has a random fingerprint that names their error reports (the login instead, if they chose to send it): see docs/telemetry.md.
// The three routes below are the person's own, for any signed-in account; the DSN and the switch stay with the owner and the admins.

type telemetryIdentityView struct {
	Fingerprint string `json:"fingerprint"` // random, stored per person, not derived from the account id
	SendName    bool   `json:"send_name"`   // reports carry the login instead of the fingerprint
	Login       string `json:"login"`       // what a report would carry with send_name on
	ReportID    string `json:"report_id"`   // the one id reports carry now (the fingerprint, or the login with send_name on; never both)
}

func identityView(id store.TelemetryIdentity) telemetryIdentityView {
	return telemetryIdentityView{Fingerprint: id.Fingerprint, SendName: id.SendName, Login: id.Login, ReportID: id.ReportID()}
}

// telemetryReportID is the id the reports of a person carry, cached because every request of a traced server asks for it.
func (s *Server) telemetryReportID(ctx context.Context, user string) (string, error) {
	if id, ok := s.telemetryIDs.Load(user); ok {
		return id.(string), nil
	}
	identity, err := s.Store.UserTelemetry(ctx, user)
	if err != nil {
		return "", err
	}
	s.telemetryIDs.Store(user, identity.ReportID())
	return identity.ReportID(), nil
}

// noteTelemetryUser is the auth service's hook: while reporting is on, the request's reports are named by their person's id. A failure to read
// it leaves the request unnamed; it never fails the request.
func (s *Server) noteTelemetryUser(ctx context.Context, user string) {
	if telemetry.Current() == nil {
		return
	}
	if id, err := s.telemetryReportID(ctx, user); err == nil {
		telemetry.SetUser(ctx, id)
	}
}

// GET /api/me/telemetry, PUT /api/me/telemetry {send_name} and POST /api/me/telemetry/fingerprint (a new random one; it also turns send_name off,
// since the new id is an anonymous one). Each answers the person's identity.
func (s *Server) telemetryIdentity(w http.ResponseWriter, r *http.Request) {
	user := userID(r)
	switch {
	case r.Method == http.MethodPut:
		var req struct {
			SendName *bool `json:"send_name"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.SendName == nil {
			apiError(w, 400, "invalid_request", "send_name is required")
			return
		}
		if err := s.Store.SetTelemetrySendName(r.Context(), user, *req.SendName); err != nil {
			fail(w, err)
			return
		}
	case r.Method == http.MethodPost:
		if _, err := s.Store.RollTelemetryFingerprint(r.Context(), user); err != nil {
			fail(w, err)
			return
		}
	}
	s.telemetryIDs.Delete(user)
	identity, err := s.Store.UserTelemetry(r.Context(), user)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, identityView(identity))
}
