package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func writeUsagesCache(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "usages")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cache := `{"providers":[{"provider_name":"claude-code","profiles":[{"profile_name":"local","active":true,"status":"ok","plan":"Max","windows":[{"key":"session","percent_used":42,"pace":{"state":"deficit","delta_percent":-8,"eta_seconds":5400}}],"credits":{"balance":3.5,"spend_limit":10}}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "cache.json"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestUsagesCommandPrintsCachedSnapshots(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("HOME", t.TempDir())
	writeUsagesCache(t, root)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"usages"}, &stdout, &stderr); code != 0 {
		t.Fatalf("usages = %d, stderr %q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"PROVIDER", "Claude Code", "ok", "Max", "session=42% (8% deficit · runs out in 1h 30m)", "3.50/10.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestUsagesPlainShowsEveryWindowAndFollowsSavedOrder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(root, "usages")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cache := `{"providers":[{"provider_name":"codex","profiles":[{"profile_name":"local","active":true,"status":"ok","windows":[{"key":"session","percent_used":1},{"key":"weekly","percent_used":2},{"key":"monthly","percent_used":3},{"key":"codex_spark","percent_used":4}]}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "cache.json"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"provider_order":["claude-code","codex"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"usages"}, &stdout, &stderr); code != 0 {
		t.Fatalf("usages = %d, stderr %q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"session=1%", "weekly=2%", "monthly=3%", "codex_spark=4%"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing window %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Claude Code") > strings.Index(out, "Codex") {
		t.Errorf("rows ignore the saved provider order:\n%s", out)
	}
}

func TestUsagesCommandJSONAndProviderFilter(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("HOME", t.TempDir())
	writeUsagesCache(t, root)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"usages", "claude-code", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("usages = %d, stderr %q", code, stderr.String())
	}
	var resp struct {
		Providers []struct {
			ProviderName string `json:"provider_name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout.String())
	}
	if len(resp.Providers) != 1 || resp.Providers[0].ProviderName != "claude-code" {
		t.Fatalf("providers = %+v", resp.Providers)
	}
}

func TestUsagesCommandArgumentErrors(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	for name, args := range map[string][]string{
		"unknown flag":     {"usages", "--bogus"},
		"both formats":     {"usages", "--json", "--plain"},
		"unknown provider": {"usages", "bogus"},
		"two providers":    {"usages", "codex", "mistral"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%s: code=%d stdout=%q stderr=%q", name, code, stdout.String(), stderr.String())
		}
	}
}

func TestUsagesCommandWithoutCacheSaysSo(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"usages"}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "no usage snapshot cached") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("usages created %d entries under the config root", len(entries))
	}
}
