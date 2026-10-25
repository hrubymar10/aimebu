package providers

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/goccy/go-json"
)

type ProviderConfig struct {
	Enabled        bool   `json:"enabled"`
	Token          string `json:"token,omitempty"`
	APIKey         string `json:"api_key,omitempty"`
	AuthMode       string `json:"auth_mode,omitempty"`
	EnterpriseHost string `json:"enterprise_host,omitempty"`
	Cookie         string `json:"cookie,omitempty"`
}

type Config struct {
	RefreshIntervalSec     int      `json:"refresh_interval_sec"`
	PercentDisplay         string   `json:"percent_display"`
	ProviderOrder          []string `json:"provider_order,omitempty"`
	AlertWarningRemaining  int      `json:"alert_warning_remaining"`
	AlertCriticalRemaining int      `json:"alert_critical_remaining"`
	// TokenAutoRefresh is the "Auto-refresh tokens" setting: on, the server owns
	// every refresh token (the token-copy model) and migrates each tool to it. It
	// was `claude_auto_refresh` before v1 renamed it; the old key is still read
	// when the new one is absent and mirrored on every save so a downgrade keeps
	// the toggle.
	TokenAutoRefresh bool `json:"token_auto_refresh"`
	// TokenAutoRefreshManaged records that the setting owns migration cleanup: a
	// manual `migrate-copies` done while the setting is off is left alone. It is
	// set when the setting is seen on (or carried over from the legacy key on) and
	// cleared once an OFF has returned every tool to the link model.
	TokenAutoRefreshManaged bool                      `json:"token_auto_refresh_managed,omitempty"`
	ClaudeModelFlag         string                    `json:"claude_model_flag"`
	CodexModelFlag          string                    `json:"codex_model_flag"`
	WarmupMode              string                    `json:"warmup_mode,omitempty"`
	WarmupSchedule          string                    `json:"warmup_schedule,omitempty"`
	Providers               map[string]ProviderConfig `json:"providers"`
}

type Store struct {
	Root   string
	Client *http.Client
}

func (s *Store) httpClient() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return usageHTTPClient
}

var ErrInvalidConfig = errors.New("invalid usage config")

func DefaultConfig() Config {
	return Config{RefreshIntervalSec: 120, PercentDisplay: "left", AlertWarningRemaining: 20, AlertCriticalRemaining: 10, ClaudeModelFlag: "--model haiku", CodexModelFlag: "--model gpt-5.6-luna", WarmupMode: "off", Providers: map[string]ProviderConfig{}}
}

func NewStore(root string) *Store { return &Store{Root: root} }
func (s *Store) LoadConfig() (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(filepath.Join(s.Root, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return DefaultConfig(), fmt.Errorf("%w: config must be an object", ErrInvalidConfig)
	}
	// A malformed warmup field should not make unrelated settings uneditable.
	// Repair only these two fields; keep structural errors elsewhere visible.
	var repair map[string]json.RawMessage
	badWarmup := false
	if json.Unmarshal(data, &repair) == nil {
		for _, key := range []string{"warmup_mode", "warmup_schedule"} {
			if raw, ok := repair[key]; ok {
				var value string
				if json.Unmarshal(raw, &value) != nil || string(raw) == "null" {
					badWarmup = true
					if key == "warmup_mode" {
						repair[key] = json.RawMessage(`"off"`)
					} else {
						repair[key] = json.RawMessage(`""`)
					}
				}
			}
		}
		data, _ = json.Marshal(repair)
	}
	err = json.Unmarshal(data, &cfg)
	if err != nil {
		return DefaultConfig(), fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	// Legacy key: only when the new one is absent, and a legacy ON owns cleanup.
	var keys map[string]json.RawMessage
	if json.Unmarshal(data, &keys) == nil {
		if _, hasNew := keys["token_auto_refresh"]; !hasNew {
			if old, hasOld := keys["claude_auto_refresh"]; hasOld {
				if err := json.Unmarshal(old, &cfg.TokenAutoRefresh); err != nil {
					return DefaultConfig(), fmt.Errorf("%w: %v", ErrInvalidConfig, err)
				}
				if cfg.TokenAutoRefresh {
					cfg.TokenAutoRefreshManaged = true
				}
			}
		}
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	if cfg.RefreshIntervalSec < 15 {
		cfg.RefreshIntervalSec = 15
	}
	if cfg.PercentDisplay != "used" {
		cfg.PercentDisplay = "left"
	}
	if err := validateAlertThresholds(cfg); err != nil {
		cfg.AlertWarningRemaining, cfg.AlertCriticalRemaining = 20, 10
	}
	cfg.ProviderOrder = normalizeProviderOrder(cfg.ProviderOrder)
	if badWarmup || ValidateWarmup(cfg.WarmupMode, cfg.WarmupSchedule) != nil {
		cfg.WarmupMode, cfg.WarmupSchedule = "off", ""
	}
	return cfg, nil
}

// UpdateConfig serializes mutations with the v1 usage lock and preserves
// config keys this v2 feature does not yet use.
func (s *Store) UpdateConfig(update func(*Config) error) (Config, error) {
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return Config{}, err
	}
	lock, err := os.OpenFile(filepath.Join(s.Root, ".lock"), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return Config{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return Config{}, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	cfg, loadErr := s.LoadConfig()
	if loadErr != nil && !errors.Is(loadErr, ErrInvalidConfig) {
		return Config{}, loadErr
	}
	if err := update(&cfg); err != nil {
		return Config{}, err
	}
	if cfg.RefreshIntervalSec < 15 {
		return Config{}, errors.New("refresh_interval_sec must be >= 15")
	}
	if cfg.PercentDisplay != "left" && cfg.PercentDisplay != "used" {
		return Config{}, errors.New("percent_display must be left or used")
	}
	if err := validateAlertThresholds(cfg); err != nil {
		return Config{}, err
	}
	if err := ValidateWarmup(cfg.WarmupMode, cfg.WarmupSchedule); err != nil {
		return Config{}, err
	}
	cfg.ProviderOrder = normalizeProviderOrder(cfg.ProviderOrder)
	path := filepath.Join(s.Root, "config.json")
	raw := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		if errors.Is(loadErr, ErrInvalidConfig) {
			// A structurally valid file can contain one field of the wrong type.
			// Refuse to overwrite the other fields, which may hold credentials.
			if json.Valid(data) {
				return Config{}, loadErr
			}
			raw = map[string]json.RawMessage{}
		} else if err := json.Unmarshal(data, &raw); err != nil {
			return Config{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}
	known, err := json.Marshal(cfg)
	if err != nil {
		return Config{}, err
	}
	var changed map[string]json.RawMessage
	if err := json.Unmarshal(known, &changed); err != nil {
		return Config{}, err
	}
	for key, value := range changed {
		raw[key] = value
	}
	// The empty schedule is omitted by JSON, so remove a stale invalid value
	// from the original file before writing the merged config.
	if cfg.WarmupSchedule == "" {
		delete(raw, "warmup_schedule")
	}
	// An omitempty field that became false is absent from `changed`, so the stale
	// true already in the file would survive the merge.
	if !cfg.TokenAutoRefreshManaged {
		delete(raw, "token_auto_refresh_managed")
	}
	// Mirror the old key name for one release so a downgrade keeps the setting.
	if mirror, err := json.Marshal(cfg.TokenAutoRefresh); err == nil {
		raw["claude_auto_refresh"] = mirror
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return Config{}, err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(s.Root, ".config-*.tmp")
	if err != nil {
		return Config{}, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return Config{}, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return Config{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Config{}, err
	}
	if err := tmp.Close(); err != nil {
		return Config{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Config{}, fmt.Errorf("save usage config: %w", err)
	}
	return cfg, nil
}

func validateAlertThresholds(cfg Config) error {
	if cfg.AlertCriticalRemaining < 1 || cfg.AlertWarningRemaining > 99 || cfg.AlertCriticalRemaining >= cfg.AlertWarningRemaining {
		return errors.New("alert remaining thresholds must be between 1 and 99, with warning above critical")
	}
	return nil
}
