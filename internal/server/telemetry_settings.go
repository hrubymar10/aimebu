package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/hrubymar10/aimebu/internal/telemetry"
)

// Settings -> Telemetry lives in three global settings; see docs/telemetry.md. They are read and written here, not in the
// store, because they only configure the telemetry package.
const (
	telemetryEnabledKey  = "telemetry_enabled"
	telemetryDSNKey      = "telemetry_dsn"
	telemetryPromptedKey = "telemetry_prompted"
)

func (s *Server) storedTelemetry(ctx context.Context) (telemetry.Stored, error) {
	var stored telemetry.Stored
	if value, ok, err := s.Store.GlobalSettingValue(ctx, telemetryEnabledKey); err != nil {
		return stored, err
	} else if ok {
		on := value == "1"
		stored.Enabled = &on
	}
	var err error
	if stored.DSN, _, err = s.Store.GlobalSettingValue(ctx, telemetryDSNKey); err != nil {
		return stored, err
	}
	prompted, _, err := s.Store.GlobalSettingValue(ctx, telemetryPromptedKey)
	stored.Prompted = prompted == "1"
	return stored, err
}

// ApplyTelemetry starts, replaces or stops error reporting from the environment and the stored settings (the environment wins). The
// server calls it at startup and after every save, so a change takes effect without a restart.
func (s *Server) ApplyTelemetry(ctx context.Context) error {
	stored, err := s.storedTelemetry(ctx)
	if err != nil {
		return err
	}
	config, err := telemetry.FromEnv(os.LookupEnv, s.Version)
	if err != nil {
		return err
	}
	config = config.Resolve(stored)
	_, err = telemetry.Init(config, s.telemetryTransport)
	s.pushTelemetry(ctx) // the supervisors report the way the server now does
	return err
}

type telemetryView struct {
	Enabled    bool   `json:"enabled"`     // the toggle: what was chosen, else on when a default DSN ships
	DSN        string `json:"dsn"`         // the field: what was chosen, else the default
	DefaultDSN string `json:"default_dsn"` // what a build ships; empty when none
	Prompted   bool   `json:"prompted"`    // the owner has answered the consent popup
	EnvLocked  bool   `json:"env_locked"`  // AIMEBU_SENTRY_DSN is set, so it wins over these fields
	Active     bool   `json:"active"`      // reports are being sent right now
	// What bounds the volume, shown read-only: the share of requests (server) and page loads (browser) that are traced, which
	// AIMEBU_SENTRY_TRACES_SAMPLE_RATE overrides, and how many database statements of one request get a span.
	TracesSampleRate float64 `json:"traces_sample_rate"`
	MaxDBSpans       int     `json:"max_db_spans"`
}

func (s *Server) telemetryView(ctx context.Context) (telemetryView, error) {
	stored, err := s.storedTelemetry(ctx)
	if err != nil {
		return telemetryView{}, err
	}
	_, envSet := os.LookupEnv("AIMEBU_SENTRY_DSN")
	rate := telemetry.DefaultTracesSampleRate
	if config, err := telemetry.FromEnv(os.LookupEnv, s.Version); err == nil { // a bad rate runs with telemetry off; the page shows the default then
		rate = config.TracesSampleRate
	}
	return telemetryView{Enabled: stored.EnabledOrDefault(), DSN: stored.DSNOrDefault(), DefaultDSN: telemetry.DefaultDSN, Prompted: stored.Prompted, EnvLocked: envSet, Active: telemetry.Current() != nil,
		TracesSampleRate: rate, MaxDBSpans: telemetry.MaxDBSpans}, nil
}

// telemetrySettings answers GET and PUT /api/telemetry/settings for the owner and admins. PUT takes any of enabled, dsn and prompted.
func (s *Server) telemetrySettings(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	if r.Method == http.MethodPut {
		var req struct {
			Enabled  *bool   `json:"enabled"`
			DSN      *string `json:"dsn"`
			Prompted *bool   `json:"prompted"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Enabled == nil && req.DSN == nil && req.Prompted == nil {
			apiError(w, 400, "invalid_request", "enabled, dsn or prompted is required")
			return
		}
		if req.DSN != nil {
			dsn := strings.TrimSpace(*req.DSN)
			if dsn != "" {
				if err := telemetry.ValidDSN(dsn); err != nil {
					apiError(w, 400, "invalid_dsn", err.Error())
					return
				}
			}
			// The default showing in the field is not a choice: storing "" keeps following the build's default (rotated, or removed with
			// default_dsn.go) instead of pinning this install to whatever it was when the owner pressed Save.
			if dsn == telemetry.DefaultDSN {
				dsn = ""
			}
			req.DSN = &dsn
		}
		flag := func(on bool) string {
			if on {
				return "1"
			}
			return "0"
		}
		writes := []struct {
			key   string
			value string
			do    bool
		}{
			{telemetryEnabledKey, "", req.Enabled != nil},
			{telemetryDSNKey, "", req.DSN != nil},
			{telemetryPromptedKey, "", req.Prompted != nil},
		}
		if req.Enabled != nil {
			writes[0].value = flag(*req.Enabled)
		}
		if req.DSN != nil {
			writes[1].value = *req.DSN
		}
		if req.Prompted != nil {
			writes[2].value = flag(*req.Prompted)
		}
		for _, write := range writes {
			if write.do {
				if err := s.Store.SetGlobalSettingValue(r.Context(), write.key, write.value); err != nil {
					fail(w, err)
					return
				}
			}
		}
		if err := s.ApplyTelemetry(r.Context()); err != nil {
			fail(w, fmt.Errorf("telemetry: %w", err))
			return
		}
	}
	view, err := s.telemetryView(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, view)
}

// telemetryPrompt says whether the consent popup is still to be shown to this user: only the owner, only until it was answered, and not
// when AIMEBU_SENTRY_DSN is set (whoever runs the server has already decided, and Settings cannot change it).
func (s *Server) telemetryPrompt(ctx context.Context, owner bool) (bool, error) {
	if _, envSet := os.LookupEnv("AIMEBU_SENTRY_DSN"); !owner || envSet {
		return false, nil
	}
	stored, err := s.storedTelemetry(ctx)
	return !stored.Prompted, err
}
