package usages

import (
	"errors"
	"net/http"

	"github.com/goccy/go-json"
)

type Routes struct {
	Manager *Manager
	Copilot *copilotFlowStore
}

func (r Routes) Mount(mux *http.ServeMux) {
	if r.Copilot == nil {
		r.Copilot = newCopilotFlowStore()
	}
	mux.HandleFunc("GET /api/usages", r.handleGet)
	mux.HandleFunc("POST /api/usages/refresh", r.handleRefresh)
	mux.HandleFunc("POST /api/usages/providers", r.handleProvider)
	mux.HandleFunc("POST /api/usages/settings", r.handleSettings)
	mux.HandleFunc("POST /api/usages/ollama/cookie", r.handleOllamaCookie)
	mux.HandleFunc("POST /api/usages/ollama/config", r.handleOllamaConfig)
	mux.HandleFunc("POST /api/usages/copilot/login/start", r.handleCopilotLoginStart)
	mux.HandleFunc("POST /api/usages/copilot/login/poll", r.handleCopilotLoginPoll)
	mux.HandleFunc("POST /api/usages/copilot/login/logout", r.handleCopilotLoginLogout)
}

func (r Routes) handleOllamaCookie(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeUsageStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	cfg, err := r.Manager.SetOllamaCookie(req.Context(), body.Cookie)
	writeUsageJSON(w, map[string]any{"config": sanitizeUsageConfig(cfg)}, err)
}

func (r Routes) handleOllamaConfig(w http.ResponseWriter, req *http.Request) {
	var body struct {
		AuthMode string  `json:"auth_mode"`
		APIKey   *string `json:"api_key"`
		Cookie   *string `json:"cookie"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeUsageStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	cfg, err := r.Manager.SetOllamaConfig(req.Context(), body.AuthMode, body.APIKey, body.Cookie)
	writeUsageJSON(w, map[string]any{"config": sanitizeUsageConfig(cfg)}, err)
}

func (r Routes) handleGet(w http.ResponseWriter, req *http.Request) {
	provider := req.URL.Query().Get("provider")
	resp, err := r.Manager.Snapshot(req.Context(), provider)
	writeUsageJSON(w, resp, err)
}

func (r Routes) handleRefresh(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Provider string `json:"provider"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	resp, retry, err := r.Manager.ForceRefresh(req.Context(), body.Provider)
	if errors.Is(err, ErrForceCooldown) {
		writeUsageStatus(w, http.StatusTooManyRequests, map[string]int{"retry_after_sec": retry})
		return
	}
	writeUsageJSON(w, resp, err)
}

func (r Routes) handleProvider(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Enabled  bool   `json:"enabled"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeUsageStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	cfg, err := r.Manager.SetProviderEnabled(req.Context(), body.Provider, body.Enabled)
	writeUsageJSON(w, map[string]any{"config": sanitizeUsageConfig(cfg)}, err)
}

func (r Routes) handleSettings(w http.ResponseWriter, req *http.Request) {
	var body struct {
		RefreshIntervalSec int       `json:"refresh_interval_sec"`
		PercentDisplay     string    `json:"percent_display"`
		ProviderOrder      *[]string `json:"provider_order"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeUsageStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	var providerOrder []string
	updateProviderOrder := body.ProviderOrder != nil
	if body.ProviderOrder != nil {
		providerOrder = *body.ProviderOrder
	}
	info, err := r.Manager.UpdateSettings(req.Context(), body.RefreshIntervalSec, body.PercentDisplay, providerOrder, updateProviderOrder)
	writeUsageJSON(w, map[string]any{"settings": info}, err)
}

func (r Routes) handleCopilotLoginStart(w http.ResponseWriter, req *http.Request) {
	var body struct {
		EnterpriseHost string `json:"enterprise_host"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	normalizedHost, err := normalizeCopilotEnterpriseHost(body.EnterpriseHost)
	if err != nil {
		writeUsageJSON(w, nil, err)
		return
	}
	resp, _, err := r.Copilot.start(req.Context(), normalizedHost)
	if err != nil {
		writeUsageJSON(w, nil, err)
		return
	}
	if err := r.Manager.store.WithLock(func() error {
		cfg, err := r.Manager.store.LoadConfig()
		if err != nil {
			return err
		}
		pc := cfg.Providers[ProviderGitHubCopilot]
		pc.EnterpriseHost = normalizedHost
		cfg.Providers[ProviderGitHubCopilot] = pc
		return r.Manager.store.SaveConfig(cfg)
	}); err != nil {
		writeUsageJSON(w, nil, err)
		return
	}
	writeUsageJSON(w, resp, nil)
}

func (r Routes) handleCopilotLoginPoll(w http.ResponseWriter, req *http.Request) {
	var body struct {
		FlowID string `json:"flow_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeUsageStatus(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	resp, token, err := r.Copilot.poll(req.Context(), body.FlowID)
	if err != nil {
		writeUsageJSON(w, nil, err)
		return
	}
	if resp.Status == "success" && token != "" {
		if err := r.Manager.store.WithLock(func() error {
			cfg, err := r.Manager.store.LoadConfig()
			if err != nil {
				return err
			}
			pc := cfg.Providers[ProviderGitHubCopilot]
			pc.Token = token
			pc.Enabled = true
			cfg.Providers[ProviderGitHubCopilot] = pc
			return r.Manager.store.SaveConfig(cfg)
		}); err != nil {
			writeUsageJSON(w, nil, err)
			return
		}
	}
	writeUsageJSON(w, resp, nil)
}

func (r Routes) handleCopilotLoginLogout(w http.ResponseWriter, req *http.Request) {
	var cfg Config
	err := r.Manager.store.WithLock(func() error {
		var err error
		cfg, err = r.Manager.store.LoadConfig()
		if err != nil {
			return err
		}
		pc := cfg.Providers[ProviderGitHubCopilot]
		pc.Token = ""
		pc.Enabled = false
		cfg.Providers[ProviderGitHubCopilot] = pc
		return r.Manager.store.SaveConfig(cfg)
	})
	writeUsageJSON(w, map[string]any{"config": sanitizeUsageConfig(cfg)}, err)
}

func sanitizeUsageConfig(cfg Config) Config {
	for key, pc := range cfg.Providers {
		pc.Token = ""
		pc.APIKey = ""
		pc.Cookie = ""
		cfg.Providers[key] = pc
	}
	return cfg
}

func writeUsageJSON(w http.ResponseWriter, data any, err error) {
	if err != nil {
		writeUsageStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeUsageStatus(w, http.StatusOK, data)
}

func writeUsageStatus(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
