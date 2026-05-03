// Package templates exposes the embedded server-side HTML templates.
// Each .html file in this directory becomes a parseable template named after
// its filename (e.g. "signup.html"). Static assets (Skydive Memory CSS +
// brand mark PNG) live under static/ and are served via StaticHandler.
package templates

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

//go:embed *.html
var files embed.FS

//go:embed static/*
var staticFS embed.FS

// funcMap exposes formatting helpers to server-side HTML templates.
// Mirrors internal/studio/ui/templates.go — when a helper is needed in
// both apps, add it to both maps to keep templates portable.
var funcMap = template.FuncMap{
	"bytesHuman": bytesHuman,
	"sub":        func(a, b int) int { return a - b },
	"barPct": func(v, max, height int) int {
		if max <= 0 {
			return 2
		}
		h := v * height / max
		if h < 2 {
			return 2
		}
		return h
	},
}

// Templates is parsed at init. Pages are tiny — no hot-reload story needed yet.
var Templates = template.Must(
	template.New("").Funcs(funcMap).ParseFS(files, "*.html"),
)

// bytesHuman: 1572864 -> "1.5 MB". Used by watch.html download cards
// and (likely) by future admin / billing pages.
func bytesHuman(n int64) string {
	if n <= 0 {
		return ""
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	const k = 1024.0
	v := float64(n)
	units := []string{"KB", "MB", "GB", "TB"}
	i := -1
	for v >= k && i < len(units)-1 {
		v /= k
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// StaticHandler serves the embedded static assets under /static/. Mount on the
// chi router with:
//
//	r.Handle("/static/*", http.StripPrefix("/static/", templates.StaticHandler()))
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("server static FS: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
