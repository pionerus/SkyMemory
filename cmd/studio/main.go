package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/pionerus/freefall/internal/config"
	"github.com/pionerus/freefall/internal/studio/ui"
)

// version is overridden at build time via -ldflags "-X main.version=..."
var version = "0.0.1-dev"

func main() {
	cfg, err := config.LoadStudio()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"version":  version,
			"platform": runtime.GOOS + "/" + runtime.GOARCH,
		})
	})

	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		data := struct {
			Version        string
			Platform       string
			Addr           string
			CloudBaseURL   string
			StatePath      string
			LicenseValid   bool
			CloudReachable bool
		}{
			Version:        version,
			Platform:       runtime.GOOS + "/" + runtime.GOARCH,
			Addr:           cfg.HTTPAddr,
			CloudBaseURL:   cfg.CloudBaseURL,
			StatePath:      cfg.StatePath,
			LicenseValid:   cfg.LicenseToken != "",
			CloudReachable: pingCloud(cfg.CloudBaseURL),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := ui.Templates.ExecuteTemplate(w, "index.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("freefall-studio v%s listening on http://%s", version, cfg.HTTPAddr)
		log.Printf("cloud: %s | state: %s", cfg.CloudBaseURL, cfg.StatePath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cncl := context.WithTimeout(context.Background(), 5*time.Second)
	defer cncl()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// pingCloud does a fast HEAD to /healthz; non-blocking on the request path because we
// already returned from page render. Only used for the homepage status pill.
func pingCloud(base string) bool {
	if base == "" {
		return false
	}
	c := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get(base + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}
