package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

const (
	harnessProvidersMax      = 16
	harnessProviderNameMax   = 80
	harnessProviderURLMax    = 512
	harnessProviderTokensMax = 10_000_000
)

// HarnessProviderNPM is the only package a custom provider may name. OpenCode npm-installs the package of a provider at run time, so
// any other name would let a saved setting run someone else's code on the machine.
const HarnessProviderNPM = adapter.CustomProviderNPM

var (
	harnessProviderIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	harnessProviderEnvPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// reservedProviderIDs are providers OpenCode ships. A custom provider by one of these ids would silently replace the built-in one for
// every model with that prefix.
var reservedProviderIDs = map[string]bool{"opencode": true, "anthropic": true, "openai": true, "google": true, "gemini": true, "mistral": true,
	"groq": true, "deepseek": true, "xai": true, "openrouter": true, "ollama": true, "ollama-cloud": true, "azure": true, "amazon-bedrock": true,
	"github-copilot": true}

// ErrInvalidHarnessProviders wraps the reason a provider list was refused.
var ErrInvalidHarnessProviders = errors.New("invalid_harness_providers")

// ErrStaleHarnessProviders is a replace that was based on a list which has changed since.
var ErrStaleHarnessProviders = errors.New("stale_harness_providers")

// HarnessProviderInUseError is a replace that would remove providers whose models are still listed.
// ProviderID and Models are the first of them; All lists every provider the replace would drop that still has models, since remove_models
// deletes the models of all of them.
type HarnessProviderInUseError struct {
	ProviderID string
	Models     int
	All        []HarnessProviderUse
}

// HarnessProviderUse is one dropped provider and the number of the harness's models that start with its id.
type HarnessProviderUse struct {
	ProviderID string
	Models     int
}

func (e *HarnessProviderInUseError) Error() string {
	if len(e.All) > 1 {
		return fmt.Sprintf("provider %q is still used by %d model(s), and %d more provider(s) are still in use", e.ProviderID, e.Models, len(e.All)-1)
	}
	return fmt.Sprintf("provider %q is still used by %d model(s)", e.ProviderID, e.Models)
}

// HarnessProvider is one custom OpenCode provider. KeyEnv names the environment variable the key is read from on the machine that runs the
// turn (empty: the endpoint takes no key); the key itself is never stored. The token limits, when set, apply to every model of the
// provider (0 leaves them to OpenCode).
type HarnessProvider struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	NPM           string `json:"npm"`
	BaseURL       string `json:"base_url"`
	KeyEnv        string `json:"key_env"`
	ContextTokens int    `json:"context_tokens,omitempty"`
	OutputTokens  int    `json:"output_tokens,omitempty"`
}

// HarnessProvidersRev names one version of a harness's provider list, as HarnessModelsRev does for its models.
func HarnessProvidersRev(providers []HarnessProvider) string {
	digest := sha256.New()
	for _, p := range providers {
		fmt.Fprintf(digest, "%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d|%d\n", len(p.ID), p.ID, len(p.Name), p.Name, len(p.NPM), p.NPM,
			len(p.BaseURL), p.BaseURL, len(p.KeyEnv), p.KeyEnv, p.ContextTokens, p.OutputTokens)
	}
	return hex.EncodeToString(digest.Sum(nil))[:16]
}

// ProtectedProviderEnv says whether a variable name is one the harness or its launcher uses itself, which a provider's key must never be
// pointed at (see adapter.ProtectedKeyEnv).
func ProtectedProviderEnv(name string) bool { return adapter.ProtectedKeyEnv(name) }

// providerEndpointAllowsPlainHTTP is true for a host a key may cross unencrypted: this machine, the Docker host alias, or a private
// network address (a local model server). Any other host must use https.
func providerEndpointAllowsPlainHTTP(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "host.docker.internal" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// ValidateHarnessProviders checks a harness's complete provider list. An empty list is always allowed.
func ValidateHarnessProviders(family string, providers []HarnessProvider) error {
	if len(providers) == 0 {
		return nil
	}
	if family != "opencode" {
		return fmt.Errorf("custom providers are only for OpenCode, not %q", family)
	}
	if len(providers) > harnessProvidersMax {
		return fmt.Errorf("at most %d providers are allowed per harness", harnessProvidersMax)
	}
	seen := map[string]bool{}
	for _, p := range providers {
		if !harnessProviderIDPattern.MatchString(p.ID) {
			return fmt.Errorf("invalid provider id %q (lowercase letters, digits and hyphens, starting with a letter, at most 40 characters)", p.ID)
		}
		if reservedProviderIDs[p.ID] {
			return fmt.Errorf("provider id %q belongs to a provider OpenCode ships; pick another", p.ID)
		}
		if seen[p.ID] {
			return fmt.Errorf("duplicate provider id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Name == "" || utf8.RuneCountInString(p.Name) > harnessProviderNameMax {
			return fmt.Errorf("provider %q needs a name of at most %d characters", p.ID, harnessProviderNameMax)
		}
		if p.NPM != HarnessProviderNPM {
			return fmt.Errorf("provider %q: npm package must be %s", p.ID, HarnessProviderNPM)
		}
		if err := validateProviderURL(p.ID, p.BaseURL); err != nil {
			return err
		}
		if p.KeyEnv != "" {
			if !harnessProviderEnvPattern.MatchString(p.KeyEnv) {
				return fmt.Errorf("provider %q: key variable %q must be an upper-case environment variable name", p.ID, p.KeyEnv)
			}
			if ProtectedProviderEnv(p.KeyEnv) {
				return fmt.Errorf("provider %q: %s is a variable the harness or its launcher uses itself and cannot hold a key", p.ID, p.KeyEnv)
			}
		}
		if p.ContextTokens < 0 || p.ContextTokens > harnessProviderTokensMax || p.OutputTokens < 0 || p.OutputTokens > harnessProviderTokensMax {
			return fmt.Errorf("provider %q: token limits must be between 0 and %d", p.ID, harnessProviderTokensMax)
		}
		if (p.ContextTokens == 0) != (p.OutputTokens == 0) {
			return fmt.Errorf("provider %q: set both token limits (context and output) or neither", p.ID)
		}
	}
	return nil
}

func validateProviderURL(id, raw string) error {
	if raw == "" || len(raw) > harnessProviderURLMax {
		return fmt.Errorf("provider %q needs a base URL of at most %d characters", id, harnessProviderURLMax)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("provider %q: %q is not a valid base URL", id, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("provider %q: the base URL must not carry credentials, a query or a fragment", id)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !providerEndpointAllowsPlainHTTP(strings.ToLower(u.Hostname())) {
			return fmt.Errorf("provider %q: plain http is only allowed for this machine, host.docker.internal or a private network address; use https", id)
		}
	default:
		return fmt.Errorf("provider %q: the base URL must start with https:// (or http:// for a local or private address)", id)
	}
	return nil
}

// HarnessProviders returns a harness's providers in the owner's order.
func (s *Store) HarnessProviders(ctx context.Context, launcherID string) ([]HarnessProvider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,npm,base_url,key_env,context_tokens,output_tokens FROM harness_provider WHERE launcher_id=? ORDER BY position`, launcherID)
	if err != nil {
		return nil, err
	}
	return scanHarnessProviders(rows)
}

// MachineHarnessProviders returns the providers of every harness of a machine, by launcher id.
func (s *Store) MachineHarnessProviders(ctx context.Context, machineID string) (map[string][]HarnessProvider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.launcher_id,p.id,p.name,p.npm,p.base_url,p.key_env,p.context_tokens,p.output_tokens
		FROM harness_provider p JOIN machine_launcher l ON l.id=p.launcher_id WHERE l.machine_id=? ORDER BY p.launcher_id,p.position`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]HarnessProvider{}
	for rows.Next() {
		var id string
		var p HarnessProvider
		if err := rows.Scan(&id, &p.ID, &p.Name, &p.NPM, &p.BaseURL, &p.KeyEnv, &p.ContextTokens, &p.OutputTokens); err != nil {
			return nil, err
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}

func scanHarnessProviders(rows *sql.Rows) ([]HarnessProvider, error) {
	defer rows.Close()
	providers := []HarnessProvider{}
	for rows.Next() {
		var p HarnessProvider
		if err := rows.Scan(&p.ID, &p.Name, &p.NPM, &p.BaseURL, &p.KeyEnv, &p.ContextTokens, &p.OutputTokens); err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, rows.Err()
}

// ReplaceHarnessProvidersIfRev replaces a harness's whole provider list atomically, on the same terms as ReplaceHarnessModelsIfRev: a
// stale expectedRev changes nothing and answers ErrStaleHarnessProviders (empty replaces unconditionally), an invalid list answers
// ErrInvalidHarnessProviders, and sql.ErrNoRows means the harness is not on that machine.
//
// A provider that is dropped (or whose id is changed, which drops the old id) while models of the harness still start with "<id>/" is
// refused with *HarnessProviderInUseError and nothing changes. removeModels makes the same replace also delete those models, in the same
// transaction; if that removes the default model another one becomes the default, so a non-empty list keeps exactly one.
func (s *Store) ReplaceHarnessProvidersIfRev(ctx context.Context, actor, machineID, launcherID string, providers []HarnessProvider, expectedRev string, removeModels bool) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var family string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT tool FROM machine_launcher WHERE id=? AND machine_id=?`, launcherID, machineID).Scan(&family); err != nil {
			return err
		}
		current, err := tx.harnessProviderList(launcherID)
		if err != nil {
			return err
		}
		if expectedRev != "" && HarnessProvidersRev(current) != expectedRev {
			return ErrStaleHarnessProviders
		}
		if err := ValidateHarnessProviders(family, providers); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidHarnessProviders, err.Error())
		}
		kept := map[string]bool{}
		for _, p := range providers {
			kept[p.ID] = true
		}
		var dropped []string
		for _, p := range current {
			if !kept[p.ID] {
				dropped = append(dropped, p.ID)
			}
		}
		var removedModels []string
		var inUse []HarnessProviderUse
		for _, id := range dropped {
			slugs, err := tx.harnessModelSlugsWithPrefix(launcherID, id+"/")
			if err != nil {
				return err
			}
			if len(slugs) > 0 {
				inUse = append(inUse, HarnessProviderUse{ProviderID: id, Models: len(slugs)})
			}
			removedModels = append(removedModels, slugs...)
		}
		if len(inUse) > 0 && !removeModels {
			return &HarnessProviderInUseError{ProviderID: inUse[0].ProviderID, Models: inUse[0].Models, All: inUse}
		}
		beforeModels := ""
		if len(removedModels) > 0 {
			if beforeModels, err = tx.harnessModelSlugs(launcherID); err != nil {
				return err
			}
			for _, slug := range removedModels {
				if _, err := tx.Exec(`DELETE FROM harness_model WHERE launcher_id=? AND slug=?`, launcherID, slug); err != nil {
					return err
				}
			}
			if err := tx.repairHarnessModelDefault(launcherID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM harness_provider WHERE launcher_id=?`, launcherID); err != nil {
			return err
		}
		for i, p := range providers {
			if _, err := tx.Exec(`INSERT INTO harness_provider(launcher_id,id,name,npm,base_url,key_env,context_tokens,output_tokens,position) VALUES (?,?,?,?,?,?,?,?,?)`,
				launcherID, p.ID, p.Name, p.NPM, p.BaseURL, p.KeyEnv, p.ContextTokens, p.OutputTokens, i); err != nil {
				return err
			}
		}
		if err := tx.launcherActivity(machineID, launcherID, actor, "providers", providerIDs(current), providerIDs(providers)); err != nil {
			return err
		}
		if len(removedModels) > 0 {
			after, err := tx.harnessModelSlugs(launcherID)
			if err != nil {
				return err
			}
			return tx.launcherActivityJSON(machineID, launcherID, actor, "models", beforeModels, after)
		}
		return nil
	})
}

func providerIDs(providers []HarnessProvider) []string {
	ids := make([]string, 0, len(providers))
	for _, p := range providers {
		ids = append(ids, p.ID)
	}
	return ids
}

func (tx *Tx) launcherActivity(machineID, launcherID, actor, action string, before, after []string) error {
	return tx.launcherActivityJSON(machineID, launcherID, actor, action, jsonList(before), jsonList(after))
}

func (tx *Tx) launcherActivityJSON(machineID, launcherID, actor, action, before, after string) error {
	id, err := newID("mla")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO machine_launcher_activity(id,machine_id,launcher_id,actor,action,before_json,after_json,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		id, machineID, launcherID, actor, action, before, after, timestamp())
	return err
}

func (tx *Tx) harnessProviderList(launcherID string) ([]HarnessProvider, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,name,npm,base_url,key_env,context_tokens,output_tokens FROM harness_provider WHERE launcher_id=? ORDER BY position`, launcherID)
	if err != nil {
		return nil, err
	}
	return scanHarnessProviders(rows)
}

func (tx *Tx) harnessModelSlugsWithPrefix(launcherID, prefix string) ([]string, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT slug FROM harness_model WHERE launcher_id=? AND available=1 AND substr(slug,1,?)=? ORDER BY position`, launcherID, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// repairHarnessModelDefault gives a non-empty model list that lost its default (the default model was removed) a new one: the first model.
func (tx *Tx) repairHarnessModelDefault(launcherID string) error {
	_, err := tx.Exec(`UPDATE harness_model SET is_default=1 WHERE launcher_id=? AND slug=(SELECT slug FROM harness_model WHERE launcher_id=? AND available=1 ORDER BY position LIMIT 1)
		AND NOT EXISTS (SELECT 1 FROM harness_model WHERE launcher_id=? AND is_default=1)`, launcherID, launcherID, launcherID)
	return err
}

// providersForTurn is the provider document an OpenCode turn carries: the one provider of the harness whose "<id>/" prefix the turn's model
// has, with the harness's models of that provider, or nothing when the model belongs to a provider OpenCode ships (or the harness is not
// OpenCode). The turn needs no other provider, so no other one is sent.
func (tx *Tx) providersForTurn(launcherID, tool, model string) ([]adapter.CustomProvider, error) {
	if tool != "opencode" || launcherID == "" {
		return nil, nil
	}
	prefix, _, found := strings.Cut(model, "/")
	if !found {
		return nil, nil
	}
	var p adapter.CustomProvider
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,name,npm,base_url,key_env,context_tokens,output_tokens FROM harness_provider WHERE launcher_id=? AND id=?`, launcherID, prefix).
		Scan(&p.ID, &p.Name, &p.NPM, &p.BaseURL, &p.KeyEnv, &p.ContextTokens, &p.OutputTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT slug,label FROM harness_model WHERE launcher_id=? AND available=1 AND substr(slug,1,?)=? ORDER BY position`, launcherID, len(prefix)+1, prefix+"/")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p.Models = []adapter.CustomProviderModel{}
	for rows.Next() {
		var slug, label string
		if err := rows.Scan(&slug, &label); err != nil {
			return nil, err
		}
		p.Models = append(p.Models, adapter.CustomProviderModel{ID: strings.TrimPrefix(slug, prefix+"/"), Name: label})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return []adapter.CustomProvider{p}, nil
}

// AgentProviderPending says the agent's next turn runs on a custom provider: it is an OpenCode agent on a harness that has the provider its
// model id names. The model is the one the turn will start with (HarnessModelFor): an agent whose model the harness no longer lists starts
// with the harness default, which may be a custom provider's. Sending such a turn to a supervisor that does not know providers would start
// OpenCode without the endpoint.
func (s *Store) AgentProviderPending(ctx context.Context, agentID string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var tool, launcherID, model string
	err := s.db.QueryRowContext(ctx, `SELECT tool,COALESCE(launcher_id,''),model FROM agent WHERE id=?`, agentID).Scan(&tool, &launcherID, &model)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (tool != "opencode" || launcherID == "") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if model, _, err = s.HarnessModelFor(ctx, launcherID, model); err != nil {
		return false, err
	}
	var pending bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_provider WHERE launcher_id=? AND substr(?,1,length(id)+1)=id||'/')`, launcherID, model).Scan(&pending)
	return pending, err
}

// HarnessCustomProviders is a harness's providers as a turn would carry them, each with the harness's models of it: what an OpenCode
// listing runs with.
func (s *Store) HarnessCustomProviders(ctx context.Context, launcherID string) ([]adapter.CustomProvider, error) {
	providers, err := s.HarnessProviders(ctx, launcherID)
	if err != nil {
		return nil, err
	}
	models, err := s.HarnessModels(ctx, launcherID)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.CustomProvider, 0, len(providers))
	for _, p := range providers {
		custom := adapter.CustomProvider{ID: p.ID, Name: p.Name, NPM: p.NPM, BaseURL: p.BaseURL, KeyEnv: p.KeyEnv, ContextTokens: p.ContextTokens, OutputTokens: p.OutputTokens,
			Models: []adapter.CustomProviderModel{}}
		for _, model := range models {
			if id, found := strings.CutPrefix(model.Slug, p.ID+"/"); found {
				custom.Models = append(custom.Models, adapter.CustomProviderModel{ID: id, Name: model.Label})
			}
		}
		out = append(out, custom)
	}
	return out, nil
}
