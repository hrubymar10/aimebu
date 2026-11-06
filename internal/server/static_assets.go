package server

import (
	"crypto/sha256"
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
}

func prepareWebAssets(files fs.FS) (webAssets, error) {
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
	version := fmt.Sprintf("%x", hash.Sum(nil)[:8])
	index, ok := raw["index.html"]
	if !ok {
		return webAssets{}, fmt.Errorf("embedded index.html is missing")
	}
	page := string(index)
	for _, path := range []string{"/v1.css", "/style.css", "/theme-v1.css", "/js/app.js"} {
		page = strings.ReplaceAll(page, path+`"`, path+"?v="+version+`"`)
	}
	page = importMap.ReplaceAllStringFunc(page, func(block string) string {
		return importMapEntry.ReplaceAllString(block, "${1}?v="+version+"${2}")
	})
	page = preloadModules(page, raw, version)
	assets := webAssets{version: version, index: []byte(page), scripts: make(map[string][]byte)}
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
