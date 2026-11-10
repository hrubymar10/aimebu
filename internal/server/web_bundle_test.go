package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

// get reads a URL of a test server, asking for gzip as a browser does when `gzipped`, and returns the response and its body as it came (not decoded).
func fetchRaw(t *testing.T, url string, gzipped bool) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gzipped {
		request.Header.Set("Accept-Encoding", "gzip")
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, body
}

// The page of the real embedded app starts from one bundle: the bundle is served minified and gzipped with its map, the page names it with the version, it
// asks the browser for no module of the app (the import map stays for the vendored libraries that are fetched on demand), and the policy is the same.
func TestThePageStartsFromTheBundle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	page, html := fetchRaw(t, f.http.URL+"/", false)
	version := regexp.MustCompile(`/bundle/app\.js\?v=([0-9a-f]+)"`).FindSubmatch(html)
	if page.StatusCode != 200 || len(version) != 2 {
		t.Fatalf("the page does not start from /bundle/app.js?v=…: status %d\n%s", page.StatusCode, html)
	}
	text := string(html)
	for _, forbidden := range []string{`src="/js/app.js`, `rel="modulepreload"`} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the page still has %s", forbidden)
		}
	}
	if !strings.Contains(text, `<script type="importmap">`) {
		t.Error("the import map is gone, and lazily fetched vendor files need it")
	}
	if policy := page.Header.Get("Content-Security-Policy"); strings.Contains(policy, "unsafe-eval") || strings.Contains(policy, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("the policy allows eval or inline script: %s", policy)
	}

	url := f.http.URL + "/bundle/app.js?v=" + string(version[1])
	plain, source := fetchRaw(t, url, false)
	if plain.StatusCode != 200 || plain.Header.Get("Content-Encoding") != "" || plain.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		!strings.HasPrefix(plain.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(plain.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("bundle: status %d, encoding %q, cache %q, type %q, vary %q", plain.StatusCode, plain.Header.Get("Content-Encoding"), plain.Header.Get("Cache-Control"), plain.Header.Get("Content-Type"), plain.Header.Get("Vary"))
	}
	// Minified (the sources are 1.3 MB with their comments and indentation; the templates' own text is kept as it is), not a stub, and no eval: the policy
	// forbids it, and a build that quietly started to need it would only fail in a browser.
	if len(source) < 200_000 || len(source) > 1_000_000 || bytes.Contains(source, []byte("eval(")) || bytes.Contains(source, []byte("new Function(")) {
		t.Fatalf("the bundle is %d bytes (want 200 KB to 1 MB), or uses eval", len(source))
	}
	if !bytes.Contains(source, []byte("//# sourceMappingURL=app.js.map")) {
		t.Error("the bundle does not name its source map")
	}
	// Function and class names survive minification (esbuild's keep-names records them as strings): an error report names them.
	for _, name := range []string{`"Chat"`, `"Sidebar"`, `"request"`} {
		if !bytes.Contains(source, []byte(name)) {
			t.Errorf("the bundle does not keep the name %s", name)
		}
	}
	// The browser's gzip: the same file, smaller, decoded by whoever asked for it.
	packed, packedBody := fetchRaw(t, url, true)
	if packed.Header.Get("Content-Encoding") != "gzip" || len(packedBody) >= len(source)/2 {
		t.Fatalf("gzip: encoding %q, %d bytes for %d", packed.Header.Get("Content-Encoding"), len(packedBody), len(source))
	}
	reader, err := gzip.NewReader(bytes.NewReader(packedBody))
	if err != nil {
		t.Fatal(err)
	}
	unpacked, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(unpacked, source) {
		t.Fatalf("the gzipped bundle is not the bundle (%v)", err)
	}
	// A request that does not carry the version is not cached for good.
	if stale, _ := fetchRaw(t, f.http.URL+"/bundle/app.js", false); stale.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("an unversioned bundle is cached: %q", stale.Header.Get("Cache-Control"))
	}
	// The source map names the app's files, not the build's.
	smap, mapBody := fetchRaw(t, f.http.URL+"/bundle/app.js.map", true)
	var parsed struct {
		Version int      `json:"version"`
		Sources []string `json:"sources"`
	}
	if smap.StatusCode != 200 || smap.Header.Get("Content-Encoding") != "" || json.Unmarshal(mapBody, &parsed) != nil || parsed.Version != 3 {
		t.Fatalf("source map: status %d, %v", smap.StatusCode, json.Unmarshal(mapBody, &parsed))
	}
	names := strings.Join(parsed.Sources, "\n")
	for _, want := range []string{"js/chat.js", "js/pages/sidebar/store.js", "vendor/preact/preact.mjs"} {
		if !strings.Contains(names, want) {
			t.Errorf("the source map does not list %s", want)
		}
	}
	for _, source := range parsed.Sources {
		if strings.Contains(source, "embedded:embedded:") {
			t.Errorf("a source of the map repeats the namespace: %s", source)
		}
		if strings.Contains(source, "/Users/") || strings.Contains(source, "/home/") || strings.Contains(source, "/tmp/") {
			t.Errorf("a source of the map names a machine path: %s", source)
		}
	}
	// The modules are still served as they were (a debugging switch and the tests of the app's internals use them).
	if module, body := fetchRaw(t, f.http.URL+"/js/api.js", false); module.StatusCode != 200 || !bytes.Contains(body, []byte("export async function request")) {
		t.Fatalf("a module is no longer served: %d", module.StatusCode)
	}
}

// With the bundle off the page starts from its modules, with the preload hints, as it did.
func TestTheBundleCanBeTurnedOffForDebugging(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.NoWebBundle = true
	server := httptest.NewServer(f.srv.Handler())
	t.Cleanup(server.Close)
	_, html := fetchRaw(t, server.URL+"/", false)
	text := string(html)
	if !regexp.MustCompile(`src="/js/app\.js\?v=[0-9a-f]+"`).MatchString(text) || !strings.Contains(text, `rel="modulepreload"`) || strings.Contains(text, "/bundle/") {
		t.Fatalf("with the bundle off the page should start from /js/app.js with preload hints:\n%s", text)
	}
	if response, _ := fetchRaw(t, server.URL+"/bundle/app.js", false); response.StatusCode != 404 && !strings.Contains(response.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("the bundle is served with the bundle off: %d", response.StatusCode)
	}
}

// forgetSaidBundleFailures empties the process-wide memory of the bundle failures already said, so a test of what is said starts from nothing.
func forgetSaidBundleFailures() {
	bundleFailuresSaid.Range(func(key, _ any) bool { bundleFailuresSaid.Delete(key); return true })
}

func TestWebBundleEnv(t *testing.T) {
	for value, want := range map[string]bool{"": true, "on": true, "1": true, "off": false, "OFF": false, " 0 ": false, "false": false, "no": false} {
		t.Setenv(webBundleEnv, value)
		if got := webBundleEnabled(); got != want {
			t.Errorf("%s=%q: enabled=%v, want %v", webBundleEnv, value, got, want)
		}
	}
}

// The bundle is an optimisation. When it cannot be made (a file that imports something that is not there, or a bare name the import map does not know) the
// app is served from its modules as it was, the failure is logged loudly and sent to Sentry when telemetry is on: a broken bundler never makes a broken page.
// This test sets the process-wide reporter, so it does not run in parallel. It also starts from, and leaves, an empty memory of the failures already said
// (a process-wide map): without that, a second run in the same process (`-count=N`) finds its failures already said and sees silence.
func TestABundleThatCannotBeMadeFallsBackToTheModulesLoudly(t *testing.T) {
	forgetSaidBundleFailures()
	defer forgetSaidBundleFailures()
	index := `<!doctype html><script type="importmap">{"imports":{"preact":"/vendor/preact/preact.mjs"}}</script><script type="module" src="/js/app.js"></script>`
	files := fstest.MapFS{
		"index.html":               {Data: []byte(index)},
		"js/app.js":                {Data: []byte(`import { x } from './missing.js'; import { h } from 'preact'; console.log(x, h);`)},
		"vendor/preact/preact.mjs": {Data: []byte(`export const h = 1;`)},
	}
	transport := &captureTransport{}
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport); err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	for _, broken := range []struct{ name, source, mention string }{
		{"a missing file", `import { x } from './missing.js'; import { h } from 'preact'; console.log(x, h);`, "missing.js"},
		{"an unknown bare import", `import 'left-pad'; import { h } from 'preact'; console.log(h);`, "left-pad"},
	} {
		files["js/app.js"] = &fstest.MapFile{Data: []byte(broken.source)}
		logged.Reset()
		assets, err := prepareWebAssets(files, true)
		if err != nil || assets.bundle != nil {
			t.Fatalf("%s: the page must be served from its modules, not fail: bundle=%v err=%v", broken.name, assets.bundle != nil, err)
		}
		page := string(assets.index)
		if !regexp.MustCompile(`src="/js/app\.js\?v=[0-9a-f]+"`).MatchString(page) || strings.Contains(page, "/bundle/") {
			t.Fatalf("%s: the page does not start from its modules: %s", broken.name, page)
		}
		if line := logged.String(); !strings.Contains(line, "WARNING") || !strings.Contains(line, "bundle could not be built") || !strings.Contains(line, broken.mention) {
			t.Fatalf("%s: the failure is not logged loudly, with its cause: %q", broken.name, line)
		}
		if _, ok := assets.scripts["js/app.js"]; !ok {
			t.Fatalf("%s: the module is not served", broken.name)
		}
	}
	// The same failure again (the handler is rebuilt for every agent MCP request) is not said again: no new log line, no new event.
	logged.Reset()
	for i := 0; i < 3; i++ {
		if _, err := prepareWebAssets(files, true); err != nil {
			t.Fatal(err)
		}
	}
	if logged.Len() != 0 {
		t.Fatalf("a failure that was already said is logged again: %q", logged.String())
	}
	telemetry.Current().Flush(time.Second)
	transport.mu.Lock()
	reported := 0
	for _, event := range transport.events {
		// An error that wraps another is reported as the chain, the cause first.
		for _, exception := range event.Exception {
			if strings.HasPrefix(exception.Value, "web bundle: ") {
				reported++
				break
			}
		}
	}
	transport.mu.Unlock()
	if reported != 2 {
		t.Fatalf("the two failures were reported to Sentry %d times", reported)
	}

	// A vendor file the page fetches on demand is not a failure: it stays the URL it is.
	files["js/missing.js"] = &fstest.MapFile{Data: []byte(`export const x = 2;`)}
	files["js/app.js"] = &fstest.MapFile{Data: []byte(`import { x } from './missing.js'; import { h } from 'preact'; export const y = import('/vendor/lazy/thing.js?v=1'); console.log(x, h);`)}
	assets, err := prepareWebAssets(files, true)
	if err != nil || assets.bundle == nil || !bytes.Contains(assets.bundle.js, []byte("/vendor/lazy/thing.js?v=1")) {
		t.Fatalf("a vendor file fetched on demand is left as its URL: %v", err)
	}
}

// The version in the page is the sources' and the bundle's: the same sources bundled differently are another version, so a cached bundle is not taken for
// the new one (an esbuild upgrade changes the output and not the sources); and a request that is not under the version can be answered 304 by its ETag.
func TestTheBundleIsPartOfTheVersionAndHasAnETag(t *testing.T) {
	t.Parallel()
	index := `<!doctype html><link rel="stylesheet" href="/style.css"><script type="importmap">{"imports":{}}</script><script type="module" src="/js/app.js"></script>`
	files := fstest.MapFS{"index.html": {Data: []byte(index)}, "style.css": {Data: []byte("a{}")}, "js/app.js": {Data: []byte(`console.log(1)`)}}
	bundled, err := prepareWebAssets(files, true)
	if err != nil || bundled.bundle == nil {
		t.Fatalf("bundle: %v", err)
	}
	modules, err := prepareWebAssets(files, false)
	if err != nil || modules.version == bundled.version {
		t.Fatalf("the bundle is not part of the version: %q for both (%v)", modules.version, err)
	}
	f := newFixture(t)
	_, html := fetchRaw(t, f.http.URL+"/", false)
	version := regexp.MustCompile(`/bundle/app\.js\?v=([0-9a-f]+)"`).FindSubmatch(html)
	if len(version) != 2 {
		t.Fatal("no versioned bundle in the page")
	}
	first, _ := fetchRaw(t, f.http.URL+"/bundle/app.js?v="+string(version[1]), true)
	tag := first.Header.Get("ETag")
	if tag == "" || tag != `"`+string(version[1])+`-gzip"` {
		t.Fatalf("the packed bundle's ETag is %q", tag)
	}
	plain, _ := fetchRaw(t, f.http.URL+"/bundle/app.js", false)
	if plain.Header.Get("ETag") == tag || plain.Header.Get("ETag") == "" {
		t.Fatalf("the plain and the packed body share an ETag: %q %q", plain.Header.Get("ETag"), tag)
	}
	request, _ := http.NewRequest(http.MethodGet, f.http.URL+"/bundle/app.js", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("If-None-Match", tag)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotModified {
		t.Fatalf("an unchanged bundle is revalidated with %d, want 304", response.StatusCode)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"": false, "gzip": true, "GZIP": true, "gzip, deflate, br": true, "deflate, gzip;q=0.8": true, "*": true,
		"gzip;q=0": false, "gzip; q=0.0": false, "br, gzip;q=0, deflate": false, "identity": false, "x-gzip": false, "gzip;q=0, *": false, "*;q=0": false,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}
