package usages

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/config"
)

const (
	EnvRefreshInterval     = "AIMEBU_USAGES_REFRESH"
	DefaultRefreshSec      = 120
	MinRefreshSec          = 15
	PercentDisplayLeft     = "left"
	PercentDisplayUsed     = "used"
	WarmupModeOff          = "off"
	WarmupModeForce        = "force"
	WarmupModeScheduled    = "scheduled"
	WarmupModeSmart        = "smart"
	DefaultClaudeModelFlag = "--model haiku"
)

var claudeModelFlagPattern = regexp.MustCompile(`^--model [A-Za-z0-9-]+$`)

type ProviderConfig struct {
	Enabled        bool   `json:"enabled"`
	Token          string `json:"token,omitempty"`
	APIKey         string `json:"api_key,omitempty"`
	AuthMode       string `json:"auth_mode,omitempty"`
	EnterpriseHost string `json:"enterprise_host,omitempty"`
	Cookie         string `json:"cookie,omitempty"`
}

type Config struct {
	RefreshIntervalSec int      `json:"refresh_interval_sec"`
	PercentDisplay     string   `json:"percent_display"`
	ProviderOrder      []string `json:"provider_order,omitempty"`
	// ClaudeAutoRefresh enables auto-refreshing near-expiry inactive claude
	// switcher profiles via an ephemeral harness-docker container. Default off:
	// enabling it incurs a small per-account cost (periodic `claude -p` calls).
	ClaudeAutoRefresh bool `json:"claude_auto_refresh,omitempty"`
	// ClaudeModelFlag is appended to the shared claude warmup/refresh prompt.
	// Empty deliberately opts into the Claude CLI's own default model.
	ClaudeModelFlag string `json:"claude_model_flag"`
	// WarmupMode selects one mutually exclusive warmup behavior. Warmup remains
	// chained to ClaudeAutoRefresh because both reuse the container/copy-back path.
	WarmupMode     string                    `json:"warmup_mode,omitempty"`
	WarmupSchedule string                    `json:"warmup_schedule,omitempty"`
	Providers      map[string]ProviderConfig `json:"providers"`
}

type CacheEntry struct {
	Profile        Profile    `json:"profile"`
	EmailFetchedAt *time.Time `json:"email_fetched_at,omitempty"`
	EmailScope     string     `json:"email_scope,omitempty"`
}

type Cache struct {
	Snapshots map[string][]CacheEntry `json:"snapshots"` // per-provider array of profile-tagged entries
}

type Store struct {
	root string
}

func NewStore() *Store {
	return NewStoreAt(filepath.Join(config.Root(), "usages"))
}

func NewStoreAt(root string) *Store {
	return &Store{root: root}
}

func (s *Store) Root() string { return s.root }

func (s *Store) ConfigPath() string { return filepath.Join(s.root, "config.json") }

func (s *Store) CachePath() string { return filepath.Join(s.root, "cache.json") }

func DefaultConfig() Config {
	providers := make(map[string]ProviderConfig, len(knownProviders))
	for _, key := range knownProviders {
		providers[key] = ProviderConfig{}
	}
	return Config{RefreshIntervalSec: DefaultRefreshSec, PercentDisplay: PercentDisplayLeft, ClaudeModelFlag: DefaultClaudeModelFlag, WarmupMode: WarmupModeOff, Providers: providers}
}

func EmptyCache() Cache {
	return Cache{Snapshots: map[string][]CacheEntry{}}
}

func (s *Store) WithLock(fn func() error) error {
	lock, err := acquireLock(s.root)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return fn()
}

func (s *Store) LoadConfig() (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return cfg, err
	}
	if _, hasMode := fields["warmup_mode"]; !hasMode {
		var legacy struct {
			ClaudeAutoWarmup   bool `json:"claude_auto_warmup"`
			WarmupSmartSpacing bool `json:"warmup_smart_spacing"`
		}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return cfg, err
		}
		switch {
		case !legacy.ClaudeAutoWarmup:
			cfg.WarmupMode = WarmupModeOff
		case legacy.WarmupSmartSpacing:
			cfg.WarmupMode = WarmupModeSmart
		case strings.TrimSpace(cfg.WarmupSchedule) != "":
			cfg.WarmupMode = WarmupModeScheduled
		default:
			cfg.WarmupMode = WarmupModeForce
		}
	}
	if _, hasModelFlag := fields["claude_model_flag"]; !hasModelFlag {
		cfg.ClaudeModelFlag = DefaultClaudeModelFlag
	}
	if !validClaudeModelFlag(cfg.ClaudeModelFlag) {
		cfg.ClaudeModelFlag = DefaultClaudeModelFlag
	}
	if !validWarmupMode(cfg.WarmupMode) {
		cfg.WarmupMode = WarmupModeOff
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	for _, key := range knownProviders {
		if _, ok := cfg.Providers[key]; !ok {
			cfg.Providers[key] = ProviderConfig{}
		}
	}
	if cfg.RefreshIntervalSec == 0 {
		cfg.RefreshIntervalSec = DefaultRefreshSec
	}
	if cfg.RefreshIntervalSec < MinRefreshSec {
		cfg.RefreshIntervalSec = MinRefreshSec
	}
	if !validPercentDisplay(cfg.PercentDisplay) {
		cfg.PercentDisplay = PercentDisplayLeft
	}
	cfg.ProviderOrder = normalizeProviderOrder(cfg.ProviderOrder)
	return cfg, nil
}

func (s *Store) SaveConfig(cfg Config) error {
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	for _, key := range knownProviders {
		if _, ok := cfg.Providers[key]; !ok {
			cfg.Providers[key] = ProviderConfig{}
		}
	}
	if cfg.RefreshIntervalSec < MinRefreshSec {
		return fmt.Errorf("refresh_interval_sec must be >= %d", MinRefreshSec)
	}
	if cfg.PercentDisplay == "" {
		cfg.PercentDisplay = PercentDisplayLeft
	}
	if !validPercentDisplay(cfg.PercentDisplay) {
		return fmt.Errorf("percent_display must be %q or %q", PercentDisplayLeft, PercentDisplayUsed)
	}
	if !validWarmupMode(cfg.WarmupMode) {
		return fmt.Errorf("warmup_mode must be off, force, scheduled, or smart")
	}
	if !validClaudeModelFlag(cfg.ClaudeModelFlag) {
		return errors.New("claude_model_flag must be empty or match --model <model-name>")
	}
	if _, err := parseWarmupSchedules(cfg.WarmupSchedule); err != nil {
		return err
	}
	cfg.ProviderOrder = normalizeProviderOrder(cfg.ProviderOrder)
	return s.writeJSONAtomic(s.ConfigPath(), cfg, 0o600)
}

func (s *Store) LoadCache() (Cache, error) {
	cache := EmptyCache()
	data, err := os.ReadFile(s.CachePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cache, nil
		}
		return cache, err
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		// A cache is best-effort state. An unreadable or schema-stale file is
		// equivalent to no cache — fall back to empty rather than propagating
		// the error, which would break every caller (GET, poller, force refresh)
		// until the file is manually deleted. Log once so a genuinely corrupt
		// file isn't silent.
		log.Printf("aimebu: usages cache unreadable, starting fresh: %v", err)
		return EmptyCache(), nil
	}
	if cache.Snapshots == nil {
		cache.Snapshots = map[string][]CacheEntry{}
	}
	return cache, nil
}

func (s *Store) SaveCache(cache Cache) error {
	if cache.Snapshots == nil {
		cache.Snapshots = map[string][]CacheEntry{}
	}
	cfg, err := s.LoadConfig()
	if err != nil {
		return err
	}
	var secrets []string
	for _, pc := range cfg.Providers {
		secrets = append(secrets, pc.Token, pc.APIKey, pc.Cookie)
	}
	for key, entries := range cache.Snapshots {
		for i := range entries {
			entries[i].Profile = entries[i].Profile.Redacted(secrets...)
		}
		cache.Snapshots[key] = entries
	}
	return s.writeJSONAtomic(s.CachePath(), cache, 0o644)
}

func (s *Store) RefreshInterval(cfg Config) (time.Duration, Settings) {
	info := Settings{
		RefreshIntervalSec: cfg.RefreshIntervalSec,
		MinRefreshSec:      MinRefreshSec,
		PercentDisplay:     cfg.PercentDisplay,
		ClaudeAutoRefresh:  cfg.ClaudeAutoRefresh,
		ClaudeModelFlag:    cfg.ClaudeModelFlag,
		WarmupMode:         cfg.WarmupMode,
		WarmupSchedule:     cfg.WarmupSchedule,
	}
	if raw := os.Getenv(EnvRefreshInterval); raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil {
			if sec < MinRefreshSec {
				sec = MinRefreshSec
			}
			info.RefreshIntervalSec = sec
			info.EnvOverride = true
			info.EnvValue = raw
		}
	}
	if info.RefreshIntervalSec < MinRefreshSec {
		info.RefreshIntervalSec = MinRefreshSec
	}
	return time.Duration(info.RefreshIntervalSec) * time.Second, info
}

func validWarmupMode(mode string) bool {
	switch mode {
	case WarmupModeOff, WarmupModeForce, WarmupModeScheduled, WarmupModeSmart:
		return true
	default:
		return false
	}
}

func validClaudeModelFlag(value string) bool {
	return value == "" || claudeModelFlagPattern.MatchString(value)
}

func validPercentDisplay(value string) bool {
	return value == PercentDisplayLeft || value == PercentDisplayUsed
}

func (s *Store) writeJSONAtomic(path string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	_ = fsyncDir(filepath.Dir(path))
	return os.Chmod(path, mode)
}

func fsyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
