package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

func (s *Server) usageReady(w http.ResponseWriter, r *http.Request) bool {
	if s.Usages == nil {
		apiError(w, 503, "unavailable", "usage snapshots unavailable")
		return false
	}
	return s.ownerMachine(w, r)
}
func (s *Server) usageError(w http.ResponseWriter, err error) {
	apiError(w, 400, "usage_config", err.Error())
}
func (s *Server) refreshUsageAfterChange(provider string, before, after providers.ProviderConfig) {
	if before == after {
		// Nothing to refetch, but the first write of an entry can still change
		// what Snapshot shows (a provider that was on by default).
		s.Usages.NotifyChanged()
		return
	}
	latest, err := s.Usages.ExtraStore().LoadConfig()
	if err != nil || latest.Providers[provider] != after {
		return
	}
	reset := before.Token != after.Token || before.Cookie != after.Cookie || before.APIKey != after.APIKey || before.AuthMode != after.AuthMode || before.EnterpriseHost != after.EnterpriseHost
	s.Usages.ConfigChanged(provider, after.Enabled, reset)
}
func (s *Server) usageProvider(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w, r) {
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Enabled  bool   `json:"enabled"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !providers.KnownProvider(body.Provider) {
		apiError(w, 400, "invalid_provider", "unknown usage provider")
		return
	}
	var before providers.ProviderConfig
	afterConfig, err := s.Usages.ExtraStore().UpdateConfig(func(cfg *providers.Config) error {
		pc := cfg.Providers[body.Provider]
		before = pc
		if body.Enabled {
			switch body.Provider {
			case providers.ProviderGitHubCopilot:
				if strings.TrimSpace(pc.Token) == "" {
					return errors.New("sign in with GitHub first")
				}
			case providers.ProviderMistral:
				if strings.TrimSpace(pc.Cookie) == "" {
					return errors.New("configure Mistral cookie first")
				}
			case providers.ProviderOllamaCloud:
				switch providers.NormalizeOllamaAuthMode(pc.AuthMode) {
				case providers.OllamaAuthCookie:
					if pc.Cookie == "" {
						return errors.New("configure Ollama cookie first")
					}
				case providers.OllamaAuthAPIKey:
					if pc.APIKey == "" {
						return errors.New("configure Ollama API key first")
					}
				default:
					if pc.Cookie == "" && pc.APIKey == "" {
						return errors.New("configure Ollama credentials first")
					}
				}
			}
		}
		pc.Enabled = body.Enabled
		cfg.Providers[body.Provider] = pc
		return nil
	})
	if err != nil {
		s.usageError(w, err)
		return
	}
	s.refreshUsageAfterChange(body.Provider, before, afterConfig.Providers[body.Provider])
	writeJSON(w, 200, s.Usages.Snapshot())
}
func (s *Server) usageSettings(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w, r) {
		return
	}
	var body struct {
		RefreshIntervalSec     *int      `json:"refresh_interval_sec"`
		PercentDisplay         *string   `json:"percent_display"`
		ProviderOrder          *[]string `json:"provider_order"`
		AlertWarningRemaining  *int      `json:"alert_warning_remaining"`
		AlertCriticalRemaining *int      `json:"alert_critical_remaining"`
		// The three opt-in alert toggles of the usages-ports experiment; each is
		// off by default and the UI only offers them in that experiment's B.
		AlertPaceEnabled        *bool `json:"alert_pace_enabled"`
		AlertResetNoticeEnabled *bool `json:"alert_reset_notice_enabled"`
		AlertExpiryEnabled      *bool `json:"alert_expiry_enabled"`
		// TokenAutoRefresh is the "Auto-refresh tokens" setting; ClaudeAutoRefresh is
		// its legacy name, accepted for older clients. The new key wins.
		TokenAutoRefresh  *bool   `json:"token_auto_refresh"`
		ClaudeAutoRefresh *bool   `json:"claude_auto_refresh"`
		WarmupMode        *string `json:"warmup_mode"`
		WarmupSchedule    *string `json:"warmup_schedule"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	tokenAutoRefresh := body.TokenAutoRefresh
	if tokenAutoRefresh == nil {
		tokenAutoRefresh = body.ClaudeAutoRefresh
	}
	tokenChanged := false
	_, err := s.Usages.ExtraStore().UpdateConfig(func(cfg *providers.Config) error {
		if tokenAutoRefresh != nil {
			tokenChanged = cfg.TokenAutoRefresh != *tokenAutoRefresh
			cfg.TokenAutoRefresh = *tokenAutoRefresh
			if *tokenAutoRefresh {
				cfg.TokenAutoRefreshManaged = true
			}
		}
		if body.RefreshIntervalSec != nil {
			cfg.RefreshIntervalSec = *body.RefreshIntervalSec
		}
		if body.PercentDisplay != nil {
			cfg.PercentDisplay = *body.PercentDisplay
		}
		if body.ProviderOrder != nil {
			cfg.ProviderOrder = *body.ProviderOrder
		}
		if body.AlertWarningRemaining != nil {
			cfg.AlertWarningRemaining = *body.AlertWarningRemaining
		}
		if body.AlertCriticalRemaining != nil {
			cfg.AlertCriticalRemaining = *body.AlertCriticalRemaining
		}
		if body.AlertPaceEnabled != nil {
			cfg.AlertPaceEnabled = *body.AlertPaceEnabled
		}
		if body.AlertResetNoticeEnabled != nil {
			cfg.AlertResetNoticeEnabled = *body.AlertResetNoticeEnabled
		}
		if body.AlertExpiryEnabled != nil {
			cfg.AlertExpiryEnabled = *body.AlertExpiryEnabled
		}
		if body.WarmupMode != nil {
			cfg.WarmupMode = *body.WarmupMode
		}
		if body.WarmupSchedule != nil {
			cfg.WarmupSchedule = *body.WarmupSchedule
		}
		return nil
	})
	if err != nil {
		s.usageError(w, err)
		return
	}
	if tokenChanged {
		s.refresherMu.Lock()
		setting := s.tokenSetting
		s.refresherMu.Unlock()
		if setting != nil {
			// Applying the setting migrates or un-migrates; that runs on the setting's
			// own worker. The response already shows the tools as pending.
			setting.Changed()
		}
	}
	s.Usages.NotifyChanged()
	writeJSON(w, 200, s.Usages.Snapshot())
}
func (s *Server) usageMistral(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w, r) {
		return
	}
	var body struct {
		Cookie *string `json:"cookie"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Cookie == nil {
		apiError(w, 400, "invalid_request", "cookie is required")
		return
	}
	cookie := ""
	if strings.TrimSpace(*body.Cookie) != "" {
		var err error
		cookie, err = providers.NormalizeMistralCookieHeader(*body.Cookie)
		if err != nil {
			s.usageError(w, err)
			return
		}
	}
	var before providers.ProviderConfig
	afterConfig, err := s.Usages.ExtraStore().UpdateConfig(func(cfg *providers.Config) error {
		pc := cfg.Providers[providers.ProviderMistral]
		before = pc
		pc.Cookie = cookie
		if cookie == "" {
			pc.Enabled = false
		} else {
			pc.Enabled = true
		}
		cfg.Providers[providers.ProviderMistral] = pc
		return nil
	})
	if err != nil {
		s.usageError(w, err)
		return
	}
	s.refreshUsageAfterChange(providers.ProviderMistral, before, afterConfig.Providers[providers.ProviderMistral])
	writeJSON(w, 200, s.Usages.Snapshot())
}
func (s *Server) usageOllama(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w, r) {
		return
	}
	var body struct {
		AuthMode *string `json:"auth_mode"`
		APIKey   *string `json:"api_key"`
		Cookie   *string `json:"cookie"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	cookie := body.Cookie
	if cookie != nil && strings.TrimSpace(*cookie) != "" {
		normalized, err := providers.NormalizeOllamaCookieHeader(*cookie)
		if err != nil {
			s.usageError(w, err)
			return
		}
		cookie = &normalized
	}
	var before providers.ProviderConfig
	afterConfig, err := s.Usages.ExtraStore().UpdateConfig(func(cfg *providers.Config) error {
		pc := cfg.Providers[providers.ProviderOllamaCloud]
		before = pc
		if body.AuthMode != nil {
			mode := strings.TrimSpace(*body.AuthMode)
			if mode != "auto" && mode != "cookie" && mode != "api_key" {
				return errors.New("auth_mode must be auto, cookie, or api_key")
			}
			pc.AuthMode = mode
		}
		if body.APIKey != nil {
			pc.APIKey = providers.NormalizeOllamaAPIKey(*body.APIKey)
		}
		if cookie != nil {
			pc.Cookie = *cookie
		}
		switch providers.NormalizeOllamaAuthMode(pc.AuthMode) {
		case providers.OllamaAuthCookie:
			pc.Enabled = pc.Cookie != ""
		case providers.OllamaAuthAPIKey:
			pc.Enabled = pc.APIKey != ""
		default:
			pc.Enabled = pc.Cookie != "" || pc.APIKey != ""
		}
		cfg.Providers[providers.ProviderOllamaCloud] = pc
		return nil
	})
	if err != nil {
		s.usageError(w, err)
		return
	}
	s.refreshUsageAfterChange(providers.ProviderOllamaCloud, before, afterConfig.Providers[providers.ProviderOllamaCloud])
	writeJSON(w, 200, s.Usages.Snapshot())
}
func (s *Server) usageCopilotStart(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w, r) {
		return
	}
	var body struct {
		EnterpriseHost string `json:"enterprise_host"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	host, err := providers.NormalizeCopilotEnterpriseHost(body.EnterpriseHost)
	if err != nil {
		s.usageError(w, err)
		return
	}
	response, err := s.CopilotFlow.Start(r.Context(), host)
	if err != nil {
		s.usageError(w, err)
		return
	}
	writeJSON(w, 200, response)
}
func (s *Server) usageCopilotPoll(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w, r) {
		return
	}
	var body struct {
		FlowID string `json:"flow_id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	response, token, host, err := s.CopilotFlow.Poll(r.Context(), body.FlowID)
	if err != nil {
		s.usageError(w, err)
		return
	}
	if response.Status == "success" && token != "" {
		var before providers.ProviderConfig
		afterConfig, updateErr := s.Usages.ExtraStore().UpdateConfig(func(cfg *providers.Config) error {
			pc := cfg.Providers[providers.ProviderGitHubCopilot]
			before = pc
			pc.Token = token
			pc.EnterpriseHost = host
			pc.Enabled = true
			cfg.Providers[providers.ProviderGitHubCopilot] = pc
			return nil
		})
		if updateErr != nil {
			s.usageError(w, updateErr)
			return
		}
		s.refreshUsageAfterChange(providers.ProviderGitHubCopilot, before, afterConfig.Providers[providers.ProviderGitHubCopilot])
	}
	writeJSON(w, 200, response)
}
func (s *Server) usageCopilotLogout(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w, r) {
		return
	}
	var before providers.ProviderConfig
	afterConfig, err := s.Usages.ExtraStore().UpdateConfig(func(cfg *providers.Config) error {
		pc := cfg.Providers[providers.ProviderGitHubCopilot]
		before = pc
		pc.Token = ""
		pc.Enabled = false
		cfg.Providers[providers.ProviderGitHubCopilot] = pc
		return nil
	})
	if err != nil {
		s.usageError(w, err)
		return
	}
	s.refreshUsageAfterChange(providers.ProviderGitHubCopilot, before, afterConfig.Providers[providers.ProviderGitHubCopilot])
	writeJSON(w, 200, s.Usages.Snapshot())
}
func (s *Server) usageRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w, r) {
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Provider != "" && !providers.KnownProvider(body.Provider) {
		apiError(w, 400, "invalid_provider", "unknown usage provider")
		return
	}
	if retry := s.Usages.CheckForceCooldown(); retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "cooldown", "message": "usage refresh was just run; try again shortly", "retry_after_sec": retry})
		return
	}
	switch body.Provider {
	case "":
		s.Usages.Refresh(r.Context())
	case providers.ProviderCodex, providers.ProviderClaudeCode:
		s.Usages.Refresh(r.Context())
	default:
		s.Usages.RefreshProvider(r.Context(), body.Provider)
	}
	writeJSON(w, 200, s.Usages.Snapshot())
}
