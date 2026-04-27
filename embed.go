package aimebu

import "embed"

//go:embed frontend/index.html frontend/style.css frontend/app.js frontend/icons
var FrontendFS embed.FS
