package aimebu

import "embed"

//go:embed frontend/index.html frontend/style.css frontend/app.js frontend/icons frontend/sounds-builtin
var FrontendFS embed.FS
