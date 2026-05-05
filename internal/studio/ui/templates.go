package ui

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/pionerus/freefall/internal/studio/state"
)

//go:embed *.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// StaticHandler serves the embedded static assets (Skydive Memory CSS + logo)
// under /static/. Mount on the chi router with:
//
//	r.Handle("/static/*", http.StripPrefix("/static/", ui.StaticHandler()))
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("studio static FS: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}

// funcMap exposes formatting helpers to studio's HTML templates.
// Add to this rather than computing in handlers — keeps template code declarative.
var funcMap = template.FuncMap{
	"humanKindLabel": state.HumanKindLabel,
	"durationHuman":  durationHuman,
	"trimTime":       trimTime,
	"bytesHuman":     bytesHuman,
	"dashCode":       dashCode,
}

// Templates parsed at init. Studio templates are tiny — we don't need a hot-reload story yet.
var Templates = template.Must(
	template.New("").Funcs(funcMap).ParseFS(templatesFS, "*.html"),
)

// durationHuman: 154.2 -> "2:34". 0 returns empty so the template can omit
// "duration" pills entirely (e.g. when ffprobe was missing during upload).
func durationHuman(seconds float64) string {
	if seconds <= 0 {
		return ""
	}
	s := int(seconds + 0.5)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// trimTime is durationHuman's chatty cousin — ALWAYS returns "M:SS", even for
// 0 (-> "0:00"). Use this for trim sliders where 0 is a real value the operator
// is allowed to pick.
func trimTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	s := int(seconds + 0.5)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// dashCode renders an 8-char Crockford access code as "XXXX-XXXX".
// Cloud stores it canonical (no dash); studio's projects table sometimes
// already has the dashed form (returned by the cloud register endpoint).
// This normaliser strips any existing dash first so both inputs render
// identically — used by the dashboard's Today + Past lists.
func dashCode(s string) string {
	out := make([]byte, 0, 9)
	for i := 0; i < len(s); i++ {
		if s[i] != '-' && s[i] != ' ' {
			out = append(out, s[i])
		}
	}
	if len(out) == 8 {
		return string(out[:4]) + "-" + string(out[4:])
	}
	return string(out)
}

// bytesHuman: 1572864 -> "1.5 MB". Used by clip-size pills and file lists.
func bytesHuman(n int64) string {
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
