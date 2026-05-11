package aimebu

import "embed"

//go:embed frontend/index.html frontend/style.css frontend/app.js frontend/manifest.webmanifest frontend/icons frontend/sounds-builtin
var FrontendFS embed.FS
