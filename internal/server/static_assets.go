package server

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// One module under two URLs would run twice (a second api.js session, a second copy of Preact).
var moduleImport = regexp.MustCompile(`((?:from|import)\s+['"]\.\.?/[^'"]+\.js)(['"])`)

// The modules a module needs before it can run: `import ... from './x.js'`, `export ... from './x.js'` and a bare `import './x.js'`.
var staticImport = regexp.MustCompile(`(?:from\s+|import\s+)['"](\.\.?/[^'"]+\.js)['"]`)

var (
	importMap      = regexp.MustCompile(`(?s)<script type="importmap">.*?</script>`)
	importMapEntry = regexp.MustCompile(`("/[^"?]+\.m?js)(")`)
	// What importMapEntry leaves behind once the entries are versioned.
	versionedMapEntry = regexp.MustCompile(`"(/[^"?]+\.m?js)\?v=[0-9a-f]+"`)
)

type webAssets struct {
	version string
	index   []byte
	scripts map[string][]byte
	// bundle is the page's modules as one file (nil when AIMEBU_WEB_BUNDLE turns it off, and then the page loads the modules themselves).
	bundle *webBundle
	// scriptHashes are the 'sha256-…' sources of the page's inline scripts (the theme script and the import map), which its Content-Security-Policy allows
	// by hash because it allows no inline script otherwise.
	scriptHashes []string
}

// inlineScript is a <script> element with a body of its own (one with a src has none to allow).
var inlineScript = regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)

// inlineScriptHashes lists the CSP hash source of every inline script of a page, in the form the browser computes it: the base64 SHA-256 of the exact body.
func inlineScriptHashes(page string) []string {
	var out []string
	for _, match := range inlineScript.FindAllStringSubmatch(page, -1) {
		if strings.Contains(match[1], "src=") || strings.TrimSpace(match[2]) == "" {
			continue
		}
		sum := sha256.Sum256([]byte(match[2]))
		out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return out
}

// contentSecurityPolicy is the policy of the app's own pages: what the page loads and talks to is this server and nothing else, scripts run only from
// its files or by the hash of the page's two inline ones (never eval or an injected inline script), it may not be framed, and a form or <base> cannot
// send it elsewhere. Styles may be inline (the page sets element styles and Mermaid writes its own), images may be data: or blob: (a pasted picture before
// it is uploaded), and the live event stream is a WebSocket to the host the page came from. extraConnect names what else the page may reach (the
// error-report endpoint, when one is set), and a worker may be a blob (the session recorder's).
func (a webAssets) contentSecurityPolicy(host string, extraConnect []string) string {
	connect := []string{"'self'"}
	if host != "" {
		connect = append(connect, "ws://"+host, "wss://"+host)
	}
	connect = append(connect, extraConnect...)
	script := append([]string{"'self'"}, a.scriptHashes...)
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(script, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"media-src 'self'",
		"font-src 'self' data:",
		"connect-src " + strings.Join(connect, " "),
		"worker-src 'self' blob:",
		"manifest-src 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'none'",
	}, "; ")
}

// prepareWebAssets reads the embedded web files and makes the page and the modules ready; with `bundled` the page starts from the bundle of the modules
// (see web_bundle.go), without it from the modules themselves.
func prepareWebAssets(files fs.FS, bundled bool) (webAssets, error) {
	raw := make(map[string][]byte)
	hash := sha256.New()
	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		hash.Write([]byte(name))
		hash.Write(data)
		raw[name] = data
		return nil
	})
	if err != nil {
		return webAssets{}, err
	}
	index, ok := raw["index.html"]
	if !ok {
		return webAssets{}, fmt.Errorf("embedded index.html is missing")
	}
	page := string(index)
	var bundle *webBundle
	if bundled {
		built, err := buildWebBundle(raw)
		if err != nil {
			// The bundle is an optimisation: when it cannot be made the app is served as its modules, as it was before, and the failure is said
			// loudly (the log, and Sentry when telemetry is on) rather than shipping a page that does not load.
			reportBundleFailure(err)
		} else {
			bundle = &built
			// The version is the sources' and the bundle's: the same sources bundled by another esbuild are another file, and must not be taken for the cached one.
			hash.Write(built.js)
		}
	}
	version := fmt.Sprintf("%x", hash.Sum(nil)[:8])
	for _, path := range []string{"/v1.css", "/style.css", "/theme-v1.css", "/v2-only.css", "/js/app.js"} {
		replacement := path
		if bundle != nil && path == "/js/app.js" {
			replacement = "/" + bundleURL // the page starts from the bundle, not from the module that is its first file
		}
		page = strings.ReplaceAll(page, path+`"`, replacement+"?v="+version+`"`)
	}
	page = importMap.ReplaceAllStringFunc(page, func(block string) string {
		return importMapEntry.ReplaceAllString(block, "${1}?v="+version+"${2}")
	})
	if bundle == nil {
		page = preloadModules(page, raw, version)
	}
	assets := webAssets{version: version, index: []byte(page), scripts: make(map[string][]byte), scriptHashes: inlineScriptHashes(page), bundle: bundle}
	for name, data := range raw {
		if strings.HasPrefix(name, "js/") && strings.HasSuffix(name, ".js") {
			assets.scripts[name] = moduleImport.ReplaceAll(data, []byte("${1}?v="+version+"${2}"))
		}
	}
	return assets, nil
}

// preloadModules puts a modulepreload hint in the page for every module app.js needs, directly or through others, and for the modules of the import
// map. A browser finds a module's imports only after it has fetched and parsed the module, so without the hints the page's ~240 modules arrive a
// level of the import tree at a time, each level costing a round trip; with them every fetch starts at once.
func preloadModules(page string, raw map[string][]byte, version string) string {
	const entry = "js/app.js"
	if _, ok := raw[entry]; !ok {
		return page
	}
	seen := map[string]bool{entry: true}
	queue := []string{entry}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, match := range staticImport.FindAllSubmatch(raw[name], -1) {
			dep := path.Join(path.Dir(name), string(match[1]))
			if _, ok := raw[dep]; ok && !seen[dep] {
				seen[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	urls := make([]string, 0, len(seen)+8)
	for name := range seen {
		urls = append(urls, "/"+name+"?v="+version)
	}
	for _, mapped := range versionedMapEntry.FindAllStringSubmatch(importMap.FindString(page), -1) {
		urls = append(urls, mapped[1]+"?v="+version)
	}
	sort.Strings(urls)
	var hints strings.Builder
	for _, url := range urls {
		hints.WriteString(`<link rel="modulepreload" href="` + url + `">` + "\n")
	}
	marker := `<script type="module" src="/js/app.js?v=` + version + `">`
	if !strings.Contains(page, marker) {
		return page
	}
	return strings.Replace(page, marker, hints.String()+marker, 1)
}
