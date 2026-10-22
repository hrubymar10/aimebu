package server

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticAssetsUseContentVersionAndRevalidateHTML(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	get := func(path string) (*http.Response, string) {
		t.Helper()
		response, err := http.Get(f.http.URL + path)
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
	for _, path := range []string{"/theme-v1.css", "/js/app.js"} {
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
	assets, err := prepareWebAssets(files)
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
