package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

// The page's ~240 ES modules are one bundle. Fetched as modules they are 240 requests, and over HTTP/1.1 (the usual way a dev server or a LAN
// install is reached) a cold load cost about five seconds at 100 ms of latency, nearly all of it round trips. The server bundles the embedded
// sources itself, when it starts, with esbuild used as a library: there is no build step, no generated file to commit or embed, no node, and
// `make dev` (which rebuilds the binary when a web file changes) gets a new bundle with it. The result is kept in memory and served minified with
// an external source map; the modules stay served as they were, which is what AIMEBU_WEB_BUNDLE=off serves the page from (for debugging, and for
// the tests that reach into the app's own module instances).

// webBundleEnv turns the bundle off: "off", "0", "false" or "no" serve the page from its modules.
const webBundleEnv = "AIMEBU_WEB_BUNDLE"

// webBundleEnabled says whether the page is served from the bundle (it is, unless the environment says otherwise).
func webBundleEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(webBundleEnv))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// bundleEntry is the module the page starts from; the bundle is served at bundleURL, with its map beside it.
const (
	bundleEntry = "js/app.js"
	bundleURL   = "bundle/app.js"
)

// webBundle is the bundle and what is served with it.
type webBundle struct {
	js     []byte
	jsGzip []byte
	smap   []byte
}

// bundleCache keeps a bundle for the sources it was made from: every Server of a process (a test makes hundreds) shares the one build.
var bundleCache sync.Map // sources digest -> *bundleOnce

type bundleOnce struct {
	once   sync.Once
	bundle webBundle
	err    error
}

// buildWebBundle bundles the page's modules (cached by their content, so a process does it once).
func buildWebBundle(raw map[string][]byte) (webBundle, error) {
	digest := sha256.New()
	names := make([]string, 0, len(raw))
	for name := range raw {
		if strings.HasPrefix(name, "js/") || strings.HasPrefix(name, "vendor/preact/") || name == "index.html" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		digest.Write([]byte(name))
		digest.Write(raw[name])
	}
	key := fmt.Sprintf("%x", digest.Sum(nil))
	entry, _ := bundleCache.LoadOrStore(key, &bundleOnce{})
	built := entry.(*bundleOnce)
	built.once.Do(func() { built.bundle, built.err = runEsbuild(raw) })
	return built.bundle, built.err
}

// reportBundleFailure says that the bundle could not be made and the page is served from its modules instead. It is loud on purpose: the app works,
// but its first load is slow again, and someone has to look. It is reported to Sentry too when telemetry is on.
// Each distinct failure is said once per process: the handler is built again for every agent MCP request, and each build would otherwise log and report
// the same failure again (a Sentry event per request).
var bundleFailuresSaid sync.Map // error text -> struct{}

func reportBundleFailure(err error) {
	if _, said := bundleFailuresSaid.LoadOrStore(err.Error(), struct{}{}); !said {
		announceBundleFailure(err)
	}
}

func announceBundleFailure(err error) {
	log.Printf("WARNING: the web UI bundle could not be built, so the page is served from its separate modules (slow to load): %v", err)
	if t := telemetry.Current(); t != nil {
		t.CaptureError(context.Background(), fmt.Errorf("web bundle: %w", err))
	}
}

// importMapAliases reads the page's import map (the one source of what "preact" and the others mean), so the bundle resolves a bare specifier as the page
// would, to the same vendored file.
func importMapAliases(index []byte) (map[string]string, error) {
	block := importMap.Find(index)
	if block == nil {
		return nil, fmt.Errorf("index.html has no import map")
	}
	body := block[bytes.IndexByte(block, '>')+1 : bytes.LastIndex(block, []byte("</script>"))]
	var parsed struct {
		Imports map[string]string `json:"imports"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("index.html import map: %w", err)
	}
	return parsed.Imports, nil
}

// runEsbuild bundles the embedded sources entirely in memory: the files are read from raw (a plugin answers esbuild's file system questions), nothing
// is written, and nothing outside the embedded web directory can be read.
func runEsbuild(raw map[string][]byte) (webBundle, error) {
	aliases, err := importMapAliases(raw["index.html"])
	if err != nil {
		return webBundle{}, err
	}
	memory := api.Plugin{Name: "embedded-web", Setup: func(build api.PluginBuild) {
		// Everything is a path inside the embedded web directory ("/js/app.js" and "js/app.js" are the same file); a bare specifier is the import map's.
		build.OnResolve(api.OnResolveOptions{Filter: `.*`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			spec := args.Path
			if mapped, ok := aliases[spec]; ok {
				spec = mapped
			}
			switch {
			case strings.HasPrefix(spec, "/vendor/") && !strings.HasPrefix(spec, "/vendor/preact/"):
				// A file the page fetches itself when it needs it (the editor, the highlighter): left as the URL it is.
				return api.OnResolveResult{Path: spec, External: true}, nil
			case strings.HasPrefix(spec, "/"):
				return resolveEmbedded(raw, strings.TrimPrefix(spec, "/"), spec)
			case args.Kind == api.ResolveEntryPoint:
				return resolveEmbedded(raw, strings.TrimPrefix(spec, "/"), spec)
			case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
				dir := ""
				if args.Namespace == embeddedNamespace {
					dir = path.Dir(args.Importer)
				}
				return resolveEmbedded(raw, path.Join(dir, spec), spec)
			}
			return api.OnResolveResult{}, fmt.Errorf("the page imports %q, which is neither a file of the app nor in its import map", args.Path)
		})
		build.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: embeddedNamespace}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			contents := string(raw[args.Path])
			return api.OnLoadResult{Contents: &contents, Loader: api.LoaderJS, ResolveDir: "/"}, nil
		})
	}}
	result := api.Build(api.BuildOptions{
		EntryPoints:       []string{bundleEntry},
		Bundle:            true,
		Format:            api.FormatESModule,
		Target:            api.ES2022,
		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		// Function and class names are kept: a stack frame in an error report names the function, and the names are all it can say once the lines and
		// columns are the minified ones (the source map is not uploaded anywhere).
		KeepNames: true,
		Sourcemap: api.SourceMapLinked,
		Outfile:   "app.js",
		Write:     false,
		Plugins:   []api.Plugin{memory},
		LogLevel:  api.LogLevelSilent,
		// The legal comments of the vendored libraries stay in the file (their licenses ask for it).
		LegalComments: api.LegalCommentsEndOfFile,
	})
	for _, message := range result.Errors {
		return webBundle{}, fmt.Errorf("bundling the web modules: %s%s", message.Text, locationOf(message))
	}
	var bundle webBundle
	for _, out := range result.OutputFiles {
		switch {
		case strings.HasSuffix(out.Path, ".map"):
			bundle.smap = append([]byte(nil), out.Contents...)
		case strings.HasSuffix(out.Path, ".js"):
			bundle.js = append([]byte(nil), out.Contents...)
		}
	}
	if len(bundle.js) == 0 {
		return webBundle{}, fmt.Errorf("bundling the web modules produced no output")
	}
	var packed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&packed, gzip.BestCompression)
	if err != nil {
		return webBundle{}, err
	}
	if _, err := writer.Write(bundle.js); err != nil {
		return webBundle{}, err
	}
	if err := writer.Close(); err != nil {
		return webBundle{}, err
	}
	bundle.jsGzip = packed.Bytes()
	return bundle, nil
}

// embeddedNamespace is the esbuild namespace of the embedded files.
const embeddedNamespace = "embedded"

func resolveEmbedded(raw map[string][]byte, name, spec string) (api.OnResolveResult, error) {
	name = path.Clean(name)
	if _, ok := raw[name]; !ok {
		return api.OnResolveResult{}, fmt.Errorf("%q is not a file of the app", spec)
	}
	// The path is the file's name in the embedded directory; esbuild writes the namespace in front of it in a source map ("embedded:js/app.js").
	return api.OnResolveResult{Path: name, Namespace: embeddedNamespace}, nil
}

func locationOf(message api.Message) string {
	if message.Location == nil {
		return ""
	}
	return fmt.Sprintf(" (%s:%d)", strings.TrimPrefix(message.Location.File, embeddedNamespace+":"), message.Location.Line)
}
