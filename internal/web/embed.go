package web

import "embed"

// DistFS holds the built Svelte app. A committed placeholder dist/index.html
// keeps `go build ./...` working before the first Vite build / on machines
// without node; scripts/build-web.sh (and scripts/build.sh) repopulate dist via
// `vite build`. The downstream internal/web server piece serves this FS under
// `fs.Sub(DistFS, "dist")`.
//
//go:embed all:dist
var DistFS embed.FS
