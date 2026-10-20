package aimebu

import (
	"embed"
	"io/fs"
)

//go:embed web
var web embed.FS

func Web() fs.FS {
	sub, _ := fs.Sub(web, "web")
	return sub
}
