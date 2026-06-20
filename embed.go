package aimebu

import "embed"

//go:embed frontend/index.html frontend/style.css frontend/utils.js frontend/render-markdown.js frontend/render-visual-plan.js frontend/app.js frontend/manifest.webmanifest frontend/icons frontend/sounds-builtin frontend/vendor
var FrontendFS embed.FS
