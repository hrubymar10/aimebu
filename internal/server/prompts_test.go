package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

type promptView struct {
	Slots []struct {
		Key       string `json:"key"`
		Default   string `json:"default"`
		Effective string `json:"effective"`
		Source    string `json:"source"`
		Global    *struct {
			Rev int64 `json:"rev"`
		} `json:"global"`
		Project *struct {
			Rev int64 `json:"rev"`
		} `json:"project"`
	} `json:"slots"`
	Max           int    `json:"max"`
	InlinePlan    string `json:"inline_plan_appendix"`
	CanEdit       bool   `json:"can_edit"`
	CanEditGlobal bool   `json:"can_edit_global"`
}

func (f *fixture) promptView(t *testing.T, projectID string) promptView {
	t.Helper()
	response := f.request(http.MethodGet, "/api/prompts?project_id="+url.QueryEscape(projectID), nil, false)
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("GET prompts: %d", response.StatusCode)
	}
	var view promptView
	if err := json.NewDecoder(response.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	return view
}

func (f *fixture) status(t *testing.T, method, path string, body any) int {
	t.Helper()
	response := f.request(method, path, body, true)
	defer response.Body.Close()
	return response.StatusCode
}

func TestPromptsAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	view := f.promptView(t, "")
	if len(view.Slots) != 6 || view.Max != 2000 || view.InlinePlan != "optional" || !view.CanEdit || !view.CanEditGlobal {
		t.Fatalf("initial view: %+v", view)
	}
	for _, slot := range view.Slots {
		if slot.Default == "" || slot.Source != "default" || slot.Effective != slot.Default {
			t.Fatalf("nothing saved yet: %+v", slot)
		}
	}
	if code := f.status(t, http.MethodPut, "/api/prompts/mentions", map[string]any{"value": "GLOBAL", "rev": 0}); code != 204 {
		t.Fatalf("save global: %d", code)
	}
	if code := f.status(t, http.MethodPut, "/api/prompts/mentions", map[string]any{"project_id": f.project.ID, "value": "PROJECT", "rev": 0}); code != 204 {
		t.Fatalf("save project: %d", code)
	}
	var mentions = func(projectID string) (string, string) {
		for _, slot := range f.promptView(t, projectID).Slots {
			if slot.Key == "mentions" {
				return slot.Source, slot.Effective
			}
		}
		return "", ""
	}
	if source, text := mentions(""); source != "global" || text != "GLOBAL" {
		t.Fatalf("global view: %s %s", source, text)
	}
	if source, text := mentions(f.project.ID); source != "project" || text != "PROJECT" {
		t.Fatalf("project view: %s %s", source, text)
	}
	// Refusals: unknown slot, empty or oversized value, a stale revision.
	for name, c := range map[string]struct {
		path string
		body map[string]any
		want int
	}{
		"unknown slot": {"/api/prompts/output", map[string]any{"value": "x", "rev": 0}, 400},
		"empty":        {"/api/prompts/tail", map[string]any{"value": "  ", "rev": 0}, 400},
		"too long":     {"/api/prompts/tail", map[string]any{"value": strings.Repeat("x", 2001), "rev": 0}, 400},
		"stale":        {"/api/prompts/mentions", map[string]any{"value": "x", "rev": 7}, 409},
		"bad project":  {"/api/prompts/tail", map[string]any{"project_id": "prj_missing", "value": "x", "rev": 0}, 403},
	} {
		if code := f.status(t, http.MethodPut, c.path, c.body); code != c.want {
			t.Errorf("%s: status %d, want %d", name, code, c.want)
		}
	}
	// Reset peels the project layer, then the global one; a stale revision is refused.
	if code := f.status(t, http.MethodDelete, "/api/prompts/mentions?project_id="+f.project.ID+"&rev=5", nil); code != 409 {
		t.Fatalf("stale reset: %d", code)
	}
	if code := f.status(t, http.MethodDelete, "/api/prompts/mentions?project_id="+f.project.ID+"&rev=1", nil); code != 204 {
		t.Fatalf("reset project: %d", code)
	}
	if code := f.status(t, http.MethodDelete, "/api/prompts/mentions?rev=1", nil); code != 204 {
		t.Fatalf("reset global: %d", code)
	}
	if source, _ := mentions(f.project.ID); source != "default" {
		t.Fatalf("back to the shipped text, got %s", source)
	}
	// Inline plans.
	if code := f.status(t, http.MethodPut, "/api/inline-plans", map[string]string{"value": "sometimes"}); code != 400 {
		t.Fatalf("bad inline-plans value: %d", code)
	}
	if code := f.status(t, http.MethodPut, "/api/inline-plans", map[string]string{"value": "always"}); code != 204 {
		t.Fatalf("inline-plans: %d", code)
	}
	if got := f.promptView(t, "").InlinePlan; got != "always" {
		t.Fatalf("inline plans = %s", got)
	}
	// Signed out, nothing is readable or writable.
	anonymous := newFixture(t)
	anonymous.cookie = nil
	if response := anonymous.request(http.MethodGet, "/api/prompts", nil, false); response.StatusCode != 401 {
		t.Fatalf("anonymous read: %d", response.StatusCode)
	}
}
