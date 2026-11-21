package codex

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// hostCodexAuthPath names the native host login when no explicit path is set.
func hostCodexAuthPath(spec adapter.TurnSpec) string {
	dir := adapter.EnvValue("CODEX_HOME", spec.LauncherEnvRefs)
	if dir == "" {
		dir = os.Getenv("CODEX_HOME")
	}
	if dir == "" {
		home := adapter.EnvValue("HOME", spec.LauncherEnvRefs)
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		dir = filepath.Join(home, ".codex")
	}
	return filepath.Join(dir, "auth.json")
}

// WarmAuthPath identifies the native login, or the exact login under the
// CODEX_HOME reported by the running Docker container.
func (a *Adapter) WarmAuthPath(spec adapter.TurnSpec) string {
	if a.variant() == "docker" {
		if spec.LauncherCodexHome == "" || !filepath.IsAbs(spec.LauncherCodexHome) {
			return ""
		}
		return filepath.Join(spec.LauncherCodexHome, "auth.json")
	}
	if a.variant() == "native" && a.AuthPath != "" {
		if resolved, err := filepath.EvalSymlinks(a.AuthPath); err == nil {
			return resolved
		}
		return a.AuthPath
	}
	return hostCodexAuthPath(spec)
}

type codexWarmAuth struct {
	Mode         string `json:"auth_mode"`
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	Tokens       struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Account string `json:"account_id"`
		ID      string `json:"id_token"`
	} `json:"tokens"`
}

// warmAuthAllowed refuses a long-lived process that could retain a refresh
// token. The cold exec path remains available for every refused credential.
func warmAuthAllowed(path string, spec adapter.TurnSpec) error {
	refuse := func() error {
		return fmt.Errorf("%w: Codex warm needs token-only credentials", adapter.ErrNoWarm)
	}
	auth, err := readCodexWarmAuth(path)
	if errors.Is(err, os.ErrNotExist) {
		// Codex can also use an API key supplied through the launch environment.
		if adapter.EnvValue("OPENAI_API_KEY", spec.LauncherEnvRefs) != "" || os.Getenv("OPENAI_API_KEY") != "" {
			return nil
		}
		return refuse()
	}
	if err != nil || auth.Tokens.Refresh != "" {
		return refuse()
	}
	if auth.Mode == "chatgptAuthTokens" && auth.Tokens.Access != "" && auth.Tokens.Account != "" {
		return nil
	}
	if (auth.Mode == "apikey" || auth.Mode == "apiKey" || auth.Mode == "") &&
		auth.Tokens.Access == "" && auth.Tokens.Account == "" && auth.Tokens.ID == "" &&
		(auth.OpenAIAPIKey != "" || adapter.EnvValue("OPENAI_API_KEY", spec.LauncherEnvRefs) != "" || os.Getenv("OPENAI_API_KEY") != "") {
		return nil
	}
	return refuse()
}

func readCodexWarmAuth(path string) (codexWarmAuth, error) {
	if path == "" {
		return codexWarmAuth{}, errors.New("Codex auth path unknown")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return codexWarmAuth{}, err
	}
	var auth codexWarmAuth
	if err := json.Unmarshal(data, &auth); err != nil {
		return codexWarmAuth{}, err
	}
	return auth, nil
}

func (w *codexWarmSession) refreshAuthTokens(params json.RawMessage) (any, any) {
	refuse := func() (any, any) {
		return nil, map[string]any{"code": -32000, "message": "aimebu cannot supply a newer token for the same Codex account"}
	}
	var request struct {
		PreviousAccountID string `json:"previousAccountId"`
	}
	if json.Unmarshal(params, &request) != nil {
		return refuse()
	}
	before := w.initialAuth
	if before.Mode != "chatgptAuthTokens" || before.Tokens.Refresh != "" || before.Tokens.Access == "" || before.Tokens.Account == "" {
		return refuse()
	}
	if request.PreviousAccountID != "" && request.PreviousAccountID != before.Tokens.Account {
		return refuse()
	}
	latest, err := readCodexWarmAuth(w.authPath)
	if err != nil || latest.Mode != "chatgptAuthTokens" || latest.Tokens.Refresh != "" || latest.Tokens.Account != before.Tokens.Account ||
		latest.Tokens.Access == "" || latest.Tokens.Access == before.Tokens.Access {
		return refuse()
	}
	plan := codexWarmPlanType(latest.Tokens.ID)
	result := map[string]string{"accessToken": latest.Tokens.Access, "chatgptAccountId": latest.Tokens.Account}
	if plan != "" {
		result["chatgptPlanType"] = plan
	}
	return result, nil
}

func codexWarmPlanType(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if value, ok := claims["https://api.openai.com/auth.chatgpt_plan_type"].(string); ok {
		return strings.TrimSpace(value)
	}
	if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if value, ok := nested["chatgpt_plan_type"].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	if value, ok := claims["chatgpt_plan_type"].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}
