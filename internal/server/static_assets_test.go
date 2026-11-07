package server

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hrubymar10/aimebu/internal/telemetry"
)

func TestStaticAssetsUseContentVersionAndRevalidateHTML(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// This is about the modules and how they are versioned and hinted (web_bundle_test.go is about the bundle): a handler of its own serves the page from them.
	f.srv.NoWebBundle = true
	modules := httptest.NewServer(f.srv.Handler())
	t.Cleanup(modules.Close)
	get := func(path string) (*http.Response, string) {
		t.Helper()
		response, err := http.Get(modules.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return response, string(body)
	}
	page, html := get("/")
	if page.StatusCode != http.StatusOK || page.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("index: status=%d cache=%q", page.StatusCode, page.Header.Get("Cache-Control"))
	}
	match := regexp.MustCompile(`href="/style\.css\?v=([0-9a-f]+)"`).FindStringSubmatch(html)
	if len(match) != 2 {
		t.Fatal("index has no versioned stylesheet")
	}
	version := match[1]
	for _, path := range []string{"/v1.css", "/theme-v1.css", "/v2-only.css", "/js/app.js"} {
		if !strings.Contains(html, path+"?v="+version+`"`) {
			t.Fatalf("index does not version %s", path)
		}
	}
	css, _ := get("/theme-v1.css?v=" + version)
	if css.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned stylesheet cache=%q", css.Header.Get("Cache-Control"))
	}
	script, js := get("/js/app.js?v=" + version)
	if script.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || !strings.Contains(js, "./api.js?v="+version+"'") {
		t.Fatalf("versioned script cache=%q; module imports not versioned", script.Header.Get("Cache-Control"))
	}
	plain, _ := get("/js/app.js")
	if plain.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("unversioned script cache=%q", plain.Header.Get("Cache-Control"))
	}
	// A browser refuses a module served with any type but JavaScript.
	for _, path := range []string{"/vendor/preact/preact.mjs", "/vendor/preact/hooks.mjs", "/vendor/preact/htm.mjs", "/vendor/preact/signals.mjs", "/vendor/preact/signals-core.mjs"} {
		if !strings.Contains(html, `"`+path+"?v="+version+`"`) {
			t.Fatalf("import map does not version %s", path)
		}
		runtime, _ := get(path + "?v=" + version)
		if runtime.StatusCode != http.StatusOK || !strings.HasPrefix(runtime.Header.Get("Content-Type"), "text/javascript") || runtime.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("%s: status=%d type=%q cache=%q", path, runtime.StatusCode, runtime.Header.Get("Content-Type"), runtime.Header.Get("Cache-Control"))
		}
	}
}

// One module under two URLs (versioned from app.js, bare from a page) would run twice.
func TestPrepareWebAssetsVersionsParentImportsAndImportMap(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"index.html":                 {Data: []byte(`<script type="importmap">{"imports":{"preact":"/vendor/preact/preact.mjs","preact/hooks":"/vendor/preact/hooks.mjs"}}</script><a href="/vendor/preact/preact.mjs">x</a><script type="module" src="/js/app.js"></script>`)},
		"js/app.js":                  {Data: []byte(`import { a } from './api.js';` + "\n" + `import { Page } from './pages/harnesses/page.js';`)},
		"js/ui.js":                   {Data: []byte(`import { h } from 'preact';`)},
		"js/pages/harnesses/page.js": {Data: []byte(`import { request } from '../../api.js';` + "\n" + `import { html } from "../../ui.js";` + "\n" + `import { Card } from './card.js';`)},
		"vendor/preact/preact.mjs":   {Data: []byte(`export const h = 1;`)},
	}
	assets, err := prepareWebAssets(files, false)
	if err != nil {
		t.Fatal(err)
	}
	v := "?v=" + assets.version
	page := string(assets.index)
	if !strings.Contains(page, `"preact":"/vendor/preact/preact.mjs`+v+`"`) || !strings.Contains(page, `"preact/hooks":"/vendor/preact/hooks.mjs`+v+`"`) {
		t.Fatalf("import map not versioned: %s", page)
	}
	if !strings.Contains(page, `href="/vendor/preact/preact.mjs">`) {
		t.Fatalf("a path outside the import map was rewritten: %s", page)
	}
	for name, want := range map[string][]string{
		"js/app.js":                  {`'./api.js` + v + `'`, `'./pages/harnesses/page.js` + v + `'`},
		"js/ui.js":                   {`from 'preact';`},
		"js/pages/harnesses/page.js": {`'../../api.js` + v + `'`, `"../../ui.js` + v + `"`, `'./card.js` + v + `'`},
	} {
		got := string(assets.scripts[name])
		for _, fragment := range want {
			if !strings.Contains(got, fragment) {
				t.Fatalf("%s: want %s in %s", name, fragment, got)
			}
		}
	}
}

// A browser finds a module's imports only after it has fetched and parsed it, so the page names every module app.js reaches, once, ahead of the entry
// script; a module nothing imports, a dynamic import and a path that is not a module are left out.
func TestPrepareWebAssetsPreloadsEveryModuleTheEntryReaches(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"index.html":               {Data: []byte(`<script type="importmap">{"imports":{"preact":"/vendor/preact/preact.mjs"}}</script><link rel="stylesheet" href="/style.css"><script type="module" src="/js/app.js"></script>`)},
		"js/app.js":                {Data: []byte(`import './dialog-focus.js';` + "\n" + `import { a } from './api.js';` + "\n" + `import { Page } from './pages/x/page.js';` + "\n" + `export * from './reexport.js';` + "\n" + `const lazy = () => import('./lazy.js');`)},
		"js/api.js":                {Data: []byte(`export const a = 1;`)},
		"js/dialog-focus.js":       {Data: []byte(`import { a } from './api.js';`)},
		"js/reexport.js":           {Data: []byte(`export const r = 1;`)},
		"js/lazy.js":               {Data: []byte(`export const l = 1;`)},
		"js/unused.js":             {Data: []byte(`export const u = 1;`)},
		"js/pages/x/page.js":       {Data: []byte(`import { a } from '../../api.js';` + "\n" + `import { h } from 'preact';`)},
		"vendor/preact/preact.mjs": {Data: []byte(`export const h = 1;`)},
	}
	assets, err := prepareWebAssets(files, false)
	if err != nil {
		t.Fatal(err)
	}
	page := string(assets.index)
	v := "?v=" + assets.version
	var hinted []string
	for _, match := range regexp.MustCompile(`<link rel="modulepreload" href="([^"]+)">`).FindAllStringSubmatch(page, -1) {
		hinted = append(hinted, match[1])
	}
	want := []string{"/js/api.js" + v, "/js/app.js" + v, "/js/dialog-focus.js" + v, "/js/pages/x/page.js" + v, "/js/reexport.js" + v, "/vendor/preact/preact.mjs" + v}
	if strings.Join(hinted, " ") != strings.Join(want, " ") {
		t.Fatalf("hinted %v\nwant   %v", hinted, want)
	}
	// A hint that names a URL no importer uses fetches the module twice: every hinted module must be imported under exactly that URL.
	imported := map[string]bool{"/js/app.js" + v: true} // the entry is loaded by the page itself
	for name, script := range assets.scripts {
		for _, match := range regexp.MustCompile(`(?:from|import)\s+['"](\.\.?/[^'"]+\.js\?v=[0-9a-f]+)['"]`).FindAllStringSubmatch(string(script), -1) {
			imported["/"+path.Join(path.Dir(name), match[1])] = true
		}
	}
	for _, url := range hinted {
		if !imported[url] && !strings.HasPrefix(url, "/vendor/") {
			t.Fatalf("%s is hinted but no module imports it under that URL (a bare `import './x.js'` must be versioned too)", url)
		}
	}
	if hints, entry := strings.Index(page, `rel="modulepreload"`), strings.Index(page, `<script type="module" src="/js/app.js`+v); hints < 0 || entry < hints {
		t.Fatalf("the hints must come before the entry script: %s", page)
	}
}

// The app page carries a Content-Security-Policy that allows its two inline scripts by hash and nothing else inline, and that keeps the page on this server.
func TestAppPageCarriesAContentSecurityPolicyThatMatchesItsInlineScripts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, path := range []string{"/", "/index.html", "/some/client/route"} {
		response, err := http.Get(f.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		policy := response.Header.Get("Content-Security-Policy")
		if policy == "" {
			t.Fatalf("%s: the page has no Content-Security-Policy", path)
		}
		// Every inline script of the page is allowed by the hash the browser would compute, and every hash in the policy belongs to one.
		bodies := inlineScript.FindAllStringSubmatch(string(body), -1)
		var withBody int
		for _, match := range bodies {
			if strings.Contains(match[1], "src=") {
				continue
			}
			withBody++
			sum := sha256.Sum256([]byte(match[2]))
			want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !strings.Contains(policy, want) {
				t.Fatalf("%s: the inline script %.60q is not allowed by hash %s in %q", path, match[2], want, policy)
			}
		}
		if withBody != 2 || strings.Count(policy, "'sha256-") != withBody {
			t.Fatalf("%s: %d inline scripts but %d hashes: %q", path, withBody, strings.Count(policy, "'sha256-"), policy)
		}
		host := strings.TrimPrefix(f.http.URL, "http://")
		for _, want := range []string{"default-src 'self'", "script-src 'self' 'sha256-", "connect-src 'self' ws://" + host + " wss://" + host, "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'", "form-action 'self'"} {
			if !strings.Contains(policy, want) {
				t.Fatalf("%s: the policy lacks %q: %q", path, want, policy)
			}
		}
		for _, bad := range []string{"unsafe-eval", "script-src 'unsafe-inline'", "script-src *", "default-src *", "http:", "https:"} {
			if strings.Contains(policy, bad) {
				t.Fatalf("%s: the policy allows too much (%q): %q", path, bad, policy)
			}
		}
		for header, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
			if got := response.Header.Get(header); got != want {
				t.Fatalf("%s: %s = %q, want %q", path, header, got, want)
			}
		}
	}
	// A file is not the page: it carries no policy of its own, and an API answer is not given one.
	response, err := http.Get(f.http.URL + "/style.css")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := response.Header.Get("Content-Security-Policy"); got != "" {
		t.Fatalf("a stylesheet carries a page policy: %q", got)
	}
}

func TestInlineScriptHashesIgnoreScriptsWithASourceAndEmptyOnes(t *testing.T) {
	t.Parallel()
	page := `<script>a()</script><script type="module" src="/x.js"></script><script type="importmap">{"imports":{}}</script><script>   </script>`
	got := inlineScriptHashes(page)
	if len(got) != 2 {
		t.Fatalf("hashes = %v", got)
	}
	sum := sha256.Sum256([]byte("a()"))
	if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; got[0] != want {
		t.Fatalf("first hash %s, want %s", got[0], want)
	}
}

// The page may send error reports to the ingest origin of the DSN it is told to report to, while reporting is on and not otherwise, and a
// browser error still reaches that origin under the policy (the worker Sentry's SDK uses needs blob:, which the policy allows).
func TestPolicyAllowsTheErrorReportOriginOnlyWhileReportingIsOn(t *testing.T) {
	f := newFixture(t)
	policy := func() string {
		response, err := http.Get(f.http.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.Header.Get("Content-Security-Policy")
	}
	if got := policy(); strings.Contains(got, "ingest") || strings.Contains(got, "https:") {
		t.Fatalf("reporting off: %q", got)
	}
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://public@o4510.ingest.de.sentry.io/4511", Environment: "test"}, discardTransport{}); err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()
	on := policy()
	for _, want := range []string{"connect-src 'self'", "https://o4510.ingest.de.sentry.io", "worker-src 'self' blob:", "script-src 'self' 'sha256-"} {
		if !strings.Contains(on, want) {
			t.Fatalf("reporting on lacks %q: %q", want, on)
		}
	}
	if strings.Contains(on, "public@") || strings.Contains(on, "/4511") {
		t.Fatalf("only the origin of the DSN may appear, not its key or project: %q", on)
	}
}
