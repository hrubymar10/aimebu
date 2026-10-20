package server

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

var moduleImport = regexp.MustCompile(`(from\s+['"]\./[^'"]+\.js)(['"])`)

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
	for _, path := range []string{"/style.css", "/theme-v1.css", "/js/app.js"} {
		page = strings.ReplaceAll(page, path+`"`, path+"?v="+version+`"`)
	}
	assets := webAssets{version: version, index: []byte(page), scripts: make(map[string][]byte)}
	for name, data := range raw {
		if strings.HasPrefix(name, "js/") && strings.HasSuffix(name, ".js") {
			assets.scripts[name] = moduleImport.ReplaceAll(data, []byte("${1}?v="+version+"${2}"))
		}
	}
	return assets, nil
}
