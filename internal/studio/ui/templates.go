package ui

import (
	"embed"
	"html/template"
)

//go:embed *.html
var templatesFS embed.FS

// Templates parsed at init. Studio templates are tiny — we don't need a hot-reload story yet.
var Templates = template.Must(template.ParseFS(templatesFS, "*.html"))
