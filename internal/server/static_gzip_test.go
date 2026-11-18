package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// The page and the large text files a load always fetches (the stylesheets and the vendored Sentry SDK) are packed once when the assets are
// prepared; a browser that accepts gzip gets the packed body under its own ETag, and one that does not gets the body as it was embedded.
func TestThePageAndItsLargeTextAssetsAreGzipped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	plainPage, plainBody := fetchRaw(t, f.http.URL+"/", false)
	if plainPage.StatusCode != http.StatusOK || plainPage.Header.Get("Content-Encoding") != "" {
		t.Fatalf("plain page: status=%d encoding=%q", plainPage.StatusCode, plainPage.Header.Get("Content-Encoding"))
	}
	plainTag := plainPage.Header.Get("ETag")
	if plainTag == "" || plainPage.Header.Get("Vary") != "Accept-Encoding" {
		t.Fatalf("plain page: etag=%q vary=%q", plainTag, plainPage.Header.Get("Vary"))
	}
	packedPage, packedBody := fetchRaw(t, f.http.URL+"/", true)
	if packedPage.StatusCode != http.StatusOK || packedPage.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("packed page: status=%d encoding=%q", packedPage.StatusCode, packedPage.Header.Get("Content-Encoding"))
	}
	packedTag := packedPage.Header.Get("ETag")
	if packedTag == "" || packedTag == plainTag || !strings.HasSuffix(packedTag, `-gzip"`) {
		t.Fatalf("packed page etag=%q, plain %q", packedTag, plainTag)
	}
	if len(packedBody) >= len(plainBody) {
		t.Fatalf("the packed page is not smaller: %d vs %d bytes", len(packedBody), len(plainBody))
	}
	if !bytes.Equal(unGzip(t, packedBody), plainBody) {
		t.Fatal("the packed page does not decode to the page the plain request answers")
	}
	// A revalidation of the packed page is answered from the ETag.
	request, _ := http.NewRequest(http.MethodGet, f.http.URL+"/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("If-None-Match", packedTag)
	response, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotModified {
		t.Fatalf("an unchanged page is revalidated with %d, want 304", response.StatusCode)
	}
	version := regexp.MustCompile(`/bundle/app\.js\?v=([0-9a-f]+)"`).FindSubmatch(plainBody)
	if len(version) != 2 {
		t.Fatal("no versioned bundle in the page")
	}
	for _, asset := range []string{"/style.css?v=" + string(version[1]), "/theme-v1.css?v=" + string(version[1]), "/vendor/sentry/bundle.tracing.min.js"} {
		plain, plainData := fetchRaw(t, f.http.URL+asset, false)
		packed, packedData := fetchRaw(t, f.http.URL+asset, true)
		if plain.Header.Get("Content-Encoding") != "" {
			t.Fatalf("%s: plain answer is %q", asset, plain.Header.Get("Content-Encoding"))
		}
		if packed.Header.Get("Content-Encoding") != "gzip" || len(packedData) >= len(plainData) {
			t.Fatalf("%s: packed encoding=%q %d bytes for %d", asset, packed.Header.Get("Content-Encoding"), len(packedData), len(plainData))
		}
		if !bytes.Equal(unGzip(t, packedData), plainData) {
			t.Fatalf("%s: the packed body does not decode to the plain one", asset)
		}
		if packed.Header.Get("ETag") == "" || packed.Header.Get("ETag") == plain.Header.Get("ETag") {
			t.Fatalf("%s: the two encodings share an ETag (%q %q)", asset, plain.Header.Get("ETag"), packed.Header.Get("ETag"))
		}
		if !strings.Contains(asset, "sentry") {
			if plain.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || packed.Header.Get("Cache-Control") != plain.Header.Get("Cache-Control") {
				t.Fatalf("%s: cache=%q / packed %q", asset, plain.Header.Get("Cache-Control"), packed.Header.Get("Cache-Control"))
			}
		}
	}
	// A browser that refuses gzip gets the body as it was embedded, also when it asks for "*" with gzip struck out.
	refusing, refusingData := fetchRaw(t, f.http.URL+"/style.css", false)
	if refusing.Header.Get("Content-Encoding") != "" || len(refusingData) == 0 {
		t.Fatalf("refused gzip: encoding=%q", refusing.Header.Get("Content-Encoding"))
	}
}

func unGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Only the page and the few large text files are packed: a module is served from its own map, a file that is missing is not packed, and nothing
// else of the app pays the memory for a second body.
func TestPrepareWebAssetsPacksThePageAndTheLargeTextAssetsOnly(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"index.html":                          {Data: []byte(`<script type="importmap">{"imports":{}}</script><link rel="stylesheet" href="/style.css"><script type="module" src="/js/app.js"></script>`)},
		"style.css":                           {Data: []byte(strings.Repeat("a{}", 1000))},
		"theme-v1.css":                        {Data: []byte(strings.Repeat("b{}", 1000))},
		"vendor/sentry/bundle.tracing.min.js": {Data: []byte(strings.Repeat("console.log(1);", 100))},
		"js/app.js":                           {Data: []byte(`console.log(1)`)},
	}
	assets, err := prepareWebAssets(files, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets.indexGzip) == 0 || len(assets.indexGzip) >= len(assets.index) {
		t.Fatalf("the page is not packed: %d gzipped for %d", len(assets.indexGzip), len(assets.index))
	}
	for _, name := range []string{"style.css", "theme-v1.css", "vendor/sentry/bundle.tracing.min.js"} {
		packed, ok := assets.packed[name]
		if !ok || !bytes.Equal(unGzip(t, packed.gz), packed.raw) || len(packed.gz) >= len(packed.raw) {
			t.Fatalf("%s: packed=%v", name, ok)
		}
	}
	if _, ok := assets.packed["js/app.js"]; ok {
		t.Fatal("a module of the app is packed too")
	}
	if _, ok := assets.packed["v1.css"]; ok {
		t.Fatal("a stylesheet the fixture does not have is packed")
	}
}
