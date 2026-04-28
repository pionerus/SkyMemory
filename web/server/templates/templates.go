// Package templates exposes the embedded server-side HTML templates.
// Each .html file in this directory becomes a parseable template named after
// its filename (e.g. "signup.html").
package templates

import (
	"embed"
	"html/template"
)

//go:embed *.html
var files embed.FS

// Templates is parsed at init. Pages are tiny — no hot-reload story needed yet.
var Templates = template.Must(template.ParseFS(files, "*.html"))
