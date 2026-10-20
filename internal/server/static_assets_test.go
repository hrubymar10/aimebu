package server

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
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
}
