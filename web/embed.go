// Package web holds the UI: the arrange page template, the script, the logo and fonts, embedded in the binary.
package web

import "embed"

//go:embed arrange.html app.js fonts.css arranger.png fonts/*.woff2
var FS embed.FS
