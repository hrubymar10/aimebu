package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/opencode"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

const (
	providerConfigMax         = 1 << 20
	providerImportMax         = 32
	providerImportModels      = 200
	providerEndpointWait      = 10 * time.Second
	providerEndpointMax       = 1 << 20
	providerKeyNamesMax       = 64
	providerEndpointModelsMax = 200
)

// providerEndpointsBudget is how long all the endpoints of a model listing get together. They are asked at once, and the listing command
// has already run for up to detectModelsTimeout, so the sum stays below the 30 seconds the server waits for the answer: a listing that
// overran it came back as an error, without the endpoint problems it was meant to show. A variable so a test need not wait for it.
var providerEndpointsBudget = 8 * time.Second

var providerKeyName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// providerKeysAnswer says, for each key variable asked about, whether it has a value where this supervisor starts the launcher's turns: in
// its own environment, or through the launcher's environment references. Only whether leaves this process, never a value; a name the
// harness uses itself is not answered, so the question cannot be used to probe the supervisor's environment.
func (c *Client) providerKeysAnswer(request protocol.DetectRequest) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.DetectResult}
	if len(request.KeyNames) > providerKeyNamesMax {
		result.Code, result.Message = "invalid_request", "too many key variables"
		return result
	}
	refs := c.launcherRow(request.LauncherID).EnvRefs
	answer := protocol.ProviderKeysAnswer{Set: map[string]bool{}}
	for _, name := range request.KeyNames {
		if !providerKeyName.MatchString(name) || adapter.ProtectedKeyEnv(name) {
			continue
		}
		answer.Set[name] = adapter.EnvValue(name, refs) != ""
	}
	result.Details, _ = json.Marshal(answer)
	return result
}

// providerImportAnswer reads the custom providers the person's own OpenCode config defines, as suggestions for a harness. The config is
// the one OpenCode itself reads on this machine (XDG_CONFIG_HOME or ~/.config, opencode.json or opencode.jsonc); an agent turn never sees
// it. Only OpenAI-compatible providers can be taken over. Keys are never copied: a reference to an environment variable becomes that
// variable's name, a key written into the config as text is only flagged.
func (c *Client) providerImportAnswer() protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.DetectResult}
	answer, err := importOpencodeProviders(opencodeConfigFiles())
	if err != nil {
		result.Code, result.Message = "import_failed", err.Error()
		return result
	}
	result.Details, _ = json.Marshal(answer)
	return result
}

// opencodeConfigFiles are the places OpenCode keeps the person's own config, in the order it prefers them.
func opencodeConfigFiles() []string {
	var dirs []string
	if base := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(base) {
		dirs = append(dirs, filepath.Join(base, "opencode"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "opencode"))
	}
	var files []string
	for _, dir := range dirs {
		files = append(files, filepath.Join(dir, "opencode.json"), filepath.Join(dir, "opencode.jsonc"))
	}
	return files
}

var providerEnvReference = regexp.MustCompile(`^\{env:([A-Z][A-Z0-9_]{0,63})\}$`)

func importOpencodeProviders(candidates []string) (protocol.ProviderImportAnswer, error) {
	answer := protocol.ProviderImportAnswer{Suggestions: []protocol.ProviderSuggestion{}}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > providerConfigMax {
			return answer, fmt.Errorf("%s is larger than %d bytes", shortHome(path), providerConfigMax)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return answer, fmt.Errorf("%s could not be read", shortHome(path))
		}
		var config struct {
			Provider map[string]struct {
				NPM     string `json:"npm"`
				Name    string `json:"name"`
				Options struct {
					BaseURL string `json:"baseURL"`
					APIKey  any    `json:"apiKey"`
				} `json:"options"`
				Models map[string]struct {
					Name  string `json:"name"`
					Limit struct {
						Context int `json:"context"`
						Output  int `json:"output"`
					} `json:"limit"`
				} `json:"models"`
			} `json:"provider"`
		}
		if err := json.Unmarshal(stripJSONC(data), &config); err != nil {
			return answer, fmt.Errorf("%s is not a config OpenCode can read: %v", shortHome(path), err)
		}
		answer.Source = shortHome(path)
		ids := make([]string, 0, len(config.Provider))
		for id := range config.Provider {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			p := config.Provider[id]
			if len(answer.Suggestions) >= providerImportMax {
				answer.Skipped = append(answer.Skipped, protocol.ProviderSkipped{ID: id, Reason: "more providers than can be imported at once"})
				continue
			}
			if p.NPM != adapter.CustomProviderNPM {
				reason := "it has no custom endpoint package"
				if p.NPM != "" {
					reason = "it uses the package " + p.NPM + ", only " + adapter.CustomProviderNPM + " is supported"
				}
				answer.Skipped = append(answer.Skipped, protocol.ProviderSkipped{ID: id, Reason: reason})
				continue
			}
			if p.Options.BaseURL == "" {
				answer.Skipped = append(answer.Skipped, protocol.ProviderSkipped{ID: id, Reason: "it has no base URL"})
				continue
			}
			suggestion := protocol.ProviderSuggestion{Provider: adapter.CustomProvider{ID: id, Name: p.Name, NPM: p.NPM, BaseURL: p.Options.BaseURL, Models: []adapter.CustomProviderModel{}}}
			switch key := p.Options.APIKey.(type) {
			case nil:
			case string:
				if match := providerEnvReference.FindStringSubmatch(key); match != nil {
					suggestion.Provider.KeyEnv = match[1]
				} else if key != "" {
					suggestion.InlineKey = true // never copied: the person names a variable and sets it themselves
				}
			default:
				suggestion.InlineKey = true
			}
			ids := make([]string, 0, len(p.Models))
			for model := range p.Models {
				ids = append(ids, model)
			}
			sort.Strings(ids)
			shared, same := struct{ context, output int }{}, true
			for i, model := range ids {
				if i >= providerImportModels {
					break
				}
				m := p.Models[model]
				name := m.Name
				if name == "" {
					name = model
				}
				suggestion.Provider.Models = append(suggestion.Provider.Models, adapter.CustomProviderModel{ID: model, Name: name})
				if i == 0 {
					shared.context, shared.output = m.Limit.Context, m.Limit.Output
				} else if m.Limit.Context != shared.context || m.Limit.Output != shared.output {
					same = false
				}
			}
			if same && shared.context > 0 && shared.output > 0 {
				suggestion.Provider.ContextTokens, suggestion.Provider.OutputTokens = shared.context, shared.output
			}
			answer.Suggestions = append(answer.Suggestions, suggestion)
		}
		return answer, nil
	}
	return answer, nil
}

// shortHome shows a path under the home folder as ~/…, which is all the owner needs to find the file.
func shortHome(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// stripJSONC turns JSON with comments and trailing commas (what OpenCode's config allows) into plain JSON. Text inside strings is left alone.
func stripJSONC(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c == '"':
			start := i
			for i++; i < len(in); i++ {
				if in[i] == '\\' {
					i++
				} else if in[i] == '"' {
					break
				}
			}
			end := i + 1
			if end > len(in) {
				end = len(in)
			}
			out = append(out, in[start:end]...)
		case c == '/' && i+1 < len(in) && in[i+1] == '/':
			for i < len(in) && in[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(in) && in[i+1] == '*':
			i += 2
			for i+1 < len(in) && !(in[i] == '*' && in[i+1] == '/') {
				i++
			}
			i++
			out = append(out, ' ')
		case c == ',':
			// A comma followed only by whitespace or comments and then a closing bracket is a trailing one.
			j := i + 1
			for j < len(in) {
				switch {
				case in[j] == ' ' || in[j] == '\t' || in[j] == '\n' || in[j] == '\r':
					j++
				case in[j] == '/' && j+1 < len(in) && in[j+1] == '/':
					for j < len(in) && in[j] != '\n' {
						j++
					}
				case in[j] == '/' && j+1 < len(in) && in[j+1] == '*':
					j += 2
					for j+1 < len(in) && !(in[j] == '*' && in[j+1] == '/') {
						j++
					}
					j += 2
				default:
					goto decided
				}
			}
		decided:
			if j < len(in) && (in[j] == '}' || in[j] == ']') {
				continue
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// providerEndpointModels asks the endpoints of custom providers which models they serve (OpenAI's GET <base>/models), with the provider's
// own key if it is set here, and returns what they listed as candidates "<provider id>/<model>". A provider that cannot be asked, or that
// answers with an error, is reported by id with a short reason and nothing else: the answer is never copied, since it could echo a key.
// The key is sent only to the provider's own base URL: redirects are not followed.
func providerEndpointModels(ctx context.Context, providers []adapter.CustomProvider, refs map[string]string) ([]protocol.ModelCandidate, map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, providerEndpointsBudget)
	defer cancel()
	client := &http.Client{Timeout: providerEndpointWait, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	type answer struct {
		ids []string
		err error
	}
	answers := make([]answer, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		// A panic in an endpoint call would end the supervisor, so it is reported (when telemetry is on) and raised
		// again as always (telemetry.Go).
		telemetry.Go(ctx, func() {
			defer wg.Done()
			ids, err := askEndpoint(ctx, client, p, refs)
			answers[i] = answer{ids, err}
		})
	}
	wg.Wait()
	// The answers are read in the providers' order, so the candidates and the problems do not depend on which endpoint was quicker.
	var found []protocol.ModelCandidate
	problems := map[string]string{}
	for i, p := range providers {
		if answers[i].err != nil {
			problems[p.ID] = answers[i].err.Error()
			continue
		}
		for _, id := range answers[i].ids {
			slug := p.ID + "/" + id
			if modelID.MatchString(slug) {
				found = append(found, protocol.ModelCandidate{Slug: slug, Label: id, Note: "listed by " + p.ID + "'s own endpoint"})
			}
		}
	}
	return found, problems
}

func askEndpoint(ctx context.Context, client *http.Client, p adapter.CustomProvider, refs map[string]string) ([]string, error) {
	if p.NPM != adapter.CustomProviderNPM || !strings.HasPrefix(p.BaseURL, "https://") && !strings.HasPrefix(p.BaseURL, "http://") || p.KeyEnv != "" && adapter.ProtectedKeyEnv(p.KeyEnv) {
		return nil, errors.New("the provider is not usable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, errors.New("the base URL is not usable")
	}
	request.Header.Set("Accept", "application/json")
	if p.KeyEnv != "" {
		key := adapter.EnvValue(p.KeyEnv, refs)
		if key == "" {
			return nil, fmt.Errorf("the key variable %s is not set on this machine", p.KeyEnv)
		}
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("the endpoint was not asked in time")
		}
		return nil, errors.New("the endpoint did not answer")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("the endpoint refused the key (HTTP %d)", response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the endpoint answered HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, providerEndpointMax+1))
	if err != nil || len(body) > providerEndpointMax {
		return nil, errors.New("the endpoint's answer could not be read")
	}
	var listed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return nil, errors.New("the endpoint did not answer with a model list")
	}
	var ids []string
	seen := map[string]bool{}
	for _, item := range listed.Data {
		if item.ID != "" && !seen[item.ID] && len(ids) < providerEndpointModelsMax {
			seen[item.ID] = true
			ids = append(ids, item.ID)
		}
	}
	return ids, nil
}

// opencodeListingEnv is the environment `opencode models` runs in: the same private config an agent turn gets (the person's own config
// and project config are ignored, the custom providers are defined), so the listing is what an agent can use. The private config home is
// a fresh folder that cleanup removes: natively under the system's temporary folder, and for a container below dir (the folder the
// launcher runs in, which its container mounts), so nothing is left in a folder the owner chose and no test writes into its package
// folder. A container is handed the document and the config home through the launcher's forward list, as a turn is. cleanup is never nil.
func opencodeListingEnv(dir string, containerized bool, providers []adapter.CustomProvider) (env []string, cleanup func(), err error) {
	cleanup = func() {}
	config := map[string]any{"$schema": "https://opencode.ai/config.json", "autoupdate": false, "share": "disabled"}
	if len(providers) > 0 {
		document, _, err := opencode.ProviderDocument(providers)
		if err != nil {
			return nil, cleanup, err
		}
		config["provider"] = document
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, cleanup, err
	}
	base := ""
	if containerized {
		base = dir
	}
	private, err := os.MkdirTemp(base, ".aimebu-opencode-config-")
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(private) }
	env = []string{"OPENCODE_CONFIG_CONTENT=" + string(raw), "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_CLAUDE_CODE=1", "XDG_CONFIG_HOME=" + private}
	if containerized {
		env = append(env, "HARNESS_DOCKER_FORWARD_ENV=OPENCODE_CONFIG_CONTENT,XDG_CONFIG_HOME,OPENCODE_DISABLE_AUTOUPDATE,OPENCODE_DISABLE_PROJECT_CONFIG,OPENCODE_DISABLE_CLAUDE_CODE")
	}
	return env, cleanup, nil
}
