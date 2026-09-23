// Package web holds the UI: page templates, the script, the logo and fonts, embedded in the binary.
package web

import "embed"

//go:embed *.html app.js fonts.css arranger.png fonts/*.woff2
var FS embed.FS
