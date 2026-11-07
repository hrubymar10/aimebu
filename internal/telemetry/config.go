// Package telemetry is the error and performance reporting to Sentry (or a self-hosted Sentry). It reports only while a DSN is in
// effect (from AIMEBU2_SENTRY_DSN or Settings, and not before the owner answered the consent popup); with none, nothing is initialized,
// nothing is sent, and the browser SDK is never loaded. What may leave the machine is decided in
// scrub.go, on both the Go side and (web/js/telemetry.js) the browser side; docs/telemetry.md lists it.
package telemetry

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/getsentry/sentry-go"
)

// DefaultDSN is the project reports go to when nobody chose one. It is empty in this file: a build that ships the maintainers' DSN sets it
// from default_dsn.go, a separate file so that removing it leaves a build that is off until a DSN comes from the environment or Settings.
var DefaultDSN string

// Config is what the environment and Settings say. The zero value (no DSN) is "telemetry off".
type Config struct {
	DSN         string
	EnvSet      bool   // AIMEBU2_SENTRY_DSN is set (to a DSN, to "off", or to empty): it wins over Settings
	Source      string // where DSN came from: "env", "settings" or "pending" (Settings not answered yet); for the status line and the API
	Environment string
	Release     string
	// TracesSampleRate is the share of HTTP requests that are traced (server and browser). There is no Session Replay: the browser bundle that
	// is vendored has none (see web/vendor/README.md), because a recording cannot be scrubbed to this package's standard yet.
	TracesSampleRate float64
}

// Enabled says whether anything is to be sent.
func (c Config) Enabled() bool { return c.DSN != "" }

var invalidReleaseChars = regexp.MustCompile(`[\s/\\]+`)

// ReleaseName turns the build's version string ("aimebu v2.0.0-3-gabc123") into a Sentry release name: no program name, and none of the
// characters (spaces, slashes) a release cannot hold.
func ReleaseName(version string) string {
	version = strings.TrimSpace(version)
	if rest, ok := strings.CutPrefix(strings.ToLower(version), "aimebu"); ok {
		version = strings.TrimSpace(version[len(version)-len(rest):])
	}
	return strings.Trim(invalidReleaseChars.ReplaceAllString(version, "_"), "_.")
}

// Stored is what Settings -> General -> Error reports holds (global settings telemetry_enabled, telemetry_dsn, telemetry_prompted).
type Stored struct {
	Enabled  *bool  // nil: never chosen, so the default (on when DefaultDSN is set)
	DSN      string // empty: DefaultDSN
	Prompted bool   // the owner has answered the consent popup
}

// EnabledOrDefault is the toggle's value: what was chosen, else on exactly when a default DSN ships.
func (s Stored) EnabledOrDefault() bool {
	if s.Enabled != nil {
		return *s.Enabled
	}
	return DefaultDSN != ""
}

// DSNOrDefault is the DSN field's value: what was chosen, else the default.
func (s Stored) DSNOrDefault() string {
	if s.DSN != "" {
		return s.DSN
	}
	return DefaultDSN
}

// Resolve settles the DSN. AIMEBU2_SENTRY_DSN wins (a DSN; "off" or empty disables). Otherwise Settings decide, but nothing is reported
// until the owner has answered the consent popup (Prompted): a fresh or updated install does not send before it has asked.
func (c Config) Resolve(stored Stored) Config {
	if c.EnvSet {
		c.Source = "env"
		return c
	}
	c.DSN = ""
	switch {
	case !stored.Prompted:
		c.Source = "pending"
	default:
		c.Source = "settings"
		if stored.EnabledOrDefault() {
			c.DSN = stored.DSNOrDefault()
		}
	}
	return c
}

// ValidDSN says whether dsn is a Sentry DSN; the error is fit to show to the person who typed it.
func ValidDSN(dsn string) error {
	parsed, err := sentry.NewDsn(dsn)
	if err != nil {
		return errors.New("that is not a Sentry DSN (https://<key>@<host>/<project id>)")
	}
	// Reports carry stack traces and request paths: they travel over https, except to a Sentry on this machine.
	if parsed.GetScheme() != "https" && !loopbackHost(parsed.GetHost()) {
		return errors.New("a Sentry DSN must use https (http only for localhost)")
	}
	return nil
}

func loopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

// FromEnv reads AIMEBU2_SENTRY_DSN ("off" or empty disables), AIMEBU2_SENTRY_ENV (default "dev"), AIMEBU2_SENTRY_TRACES_SAMPLE_RATE (default 0.1). A bad rate is an error:
// the caller says so and runs with telemetry off, rather than guessing.
func FromEnv(lookup func(string) (string, bool), version string) (Config, error) {
	getenv := func(name string) string { value, _ := lookup(name); return value }
	config := Config{Environment: strings.TrimSpace(getenv("AIMEBU2_SENTRY_ENV")), Release: ReleaseName(version),
		TracesSampleRate: 0.1}
	if value, set := lookup("AIMEBU2_SENTRY_DSN"); set {
		config.EnvSet = true
		if value = strings.TrimSpace(value); !strings.EqualFold(value, "off") {
			config.DSN = value
		}
	}
	if config.Environment == "" {
		config.Environment = "dev"
	}
	for _, rate := range []struct {
		name string
		into *float64
	}{
		{"AIMEBU2_SENTRY_TRACES_SAMPLE_RATE", &config.TracesSampleRate},
	} {
		raw := strings.TrimSpace(getenv(rate.name))
		if raw == "" {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value < 0 || value > 1 {
			return Config{}, fmt.Errorf("%s must be a number from 0 to 1, got %q", rate.name, raw)
		}
		*rate.into = value
	}
	return config, nil
}
