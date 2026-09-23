// Package web holds the UI: page templates, the script and the logo, embedded in the binary.
package web

import "embed"

//go:embed *.html app.js arranger.png
var FS embed.FS
