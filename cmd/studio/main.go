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
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	v1 "github.com/pionerus/freefall/internal/api/v1"
	"github.com/pionerus/freefall/internal/config"
	"github.com/pionerus/freefall/internal/studio/jump"
	"github.com/pionerus/freefall/internal/studio/license"
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

	licenseClient := license.NewClient(cfg.CloudBaseURL)
	licenseMgr := license.NewManager(licenseClient, cfg.LicenseToken, version, 0 /* default 6h */)
	licenseMgr.Start(ctx)

	jumpClient := jump.NewClient(cfg.CloudBaseURL, cfg.LicenseToken)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		res, _ := licenseMgr.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":         "ok",
			"version":        version,
			"platform":       runtime.GOOS + "/" + runtime.GOARCH,
			"license_valid":  res.Valid,
			"license_reason": res.Reason,
		})
	})

	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		res, lastAt := licenseMgr.Snapshot()
		data := homeData{
			Version:           version,
			Platform:          runtime.GOOS + "/" + runtime.GOARCH,
			Addr:              cfg.HTTPAddr,
			CloudBaseURL:      cfg.CloudBaseURL,
			StatePath:         cfg.StatePath,
			TokenConfigured:   cfg.LicenseToken != "",
			License:           res,
			LicenseCheckedAt:  lastAt,
			CloudReachable:    pingCloud(cfg.CloudBaseURL),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := ui.Templates.ExecuteTemplate(w, "index.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Force a license re-check on demand. Useful after operator updates STUDIO_LICENSE_TOKEN
	// without restarting (note: env var is read once at boot — restart is still required to
	// pick up a NEW token; this endpoint just re-validates the existing one).
	r.Post("/license/refresh", func(w http.ResponseWriter, r *http.Request) {
		licenseMgr.Start(r.Context()) // re-trigger immediate validation; idempotent
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	// New project flow. GET renders the form, POST sends it through to the cloud
	// (POST /api/v1/jumps/register) and renders the access_code on success.
	r.Get("/projects/new", func(w http.ResponseWriter, req *http.Request) {
		res, _ := licenseMgr.Snapshot()
		if !res.Valid {
			http.Redirect(w, req, "/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = ui.Templates.ExecuteTemplate(w, "new_project.html", map[string]any{
			"License": res,
		})
	})

	r.Post("/projects", func(w http.ResponseWriter, req *http.Request) {
		res, _ := licenseMgr.Snapshot()
		if !res.Valid {
			writeStudioJSON(w, http.StatusUnauthorized, map[string]string{
				"code": "LICENSE_INVALID", "message": "License is not valid.",
			})
			return
		}

		var jr v1.JumpRegisterRequest
		if err := json.NewDecoder(req.Body).Decode(&jr); err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{
				"code": "INVALID_JSON", "message": "Could not parse form payload.",
			})
			return
		}

		callCtx, cancel := context.WithTimeout(req.Context(), 12*time.Second)
		defer cancel()

		out, err := jumpClient.Register(callCtx, jr)
		if err != nil {
			var apiErr *jump.APIError
			if errors.As(err, &apiErr) {
				writeStudioJSON(w, apiErr.HTTPStatus, map[string]string{
					"code": apiErr.Code, "message": apiErr.Message,
				})
				return
			}
			writeStudioJSON(w, http.StatusBadGateway, map[string]string{
				"code": "CLOUD_UNREACHABLE", "message": err.Error(),
			})
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = ui.Templates.ExecuteTemplate(w, "new_project_done.html", map[string]any{
			"License":                   res,
			"CloudBaseURL":              cfg.CloudBaseURL,
			"JumpID":                    out.JumpID,
			"ClientID":                  out.ClientID,
			"AccessCode":                out.AccessCode,
			"AccessCodeCanonical":       strings.ReplaceAll(out.AccessCode, "-", ""),
			"ClientName":                jr.ClientName,
			"ClientEmail":               jr.ClientEmail,
			"ClientPhone":               jr.ClientPhone,
			"Output1080p":               jr.Output1080p,
			"Output4K":                  jr.Output4K,
			"OutputVertical":            jr.OutputVertical,
			"OutputPhotos":              jr.OutputPhotos,
			"HasOperatorUploadedPhotos": jr.HasOperatorUploadedPhotos,
		})
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("freefall-studio v%s listening on http://%s", version, cfg.HTTPAddr)
		log.Printf("cloud: %s | state: %s", cfg.CloudBaseURL, cfg.StatePath)
		if cfg.LicenseToken == "" {
			log.Printf("license: STUDIO_LICENSE_TOKEN not set — pipeline will be disabled")
		}
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

// homeData is the template context for the studio home page.
type homeData struct {
	Version          string
	Platform         string
	Addr             string
	CloudBaseURL     string
	StatePath        string
	TokenConfigured  bool
	License          license.Result
	LicenseCheckedAt time.Time
	CloudReachable   bool
}

// writeStudioJSON sends a JSON response from a studio handler.
func writeStudioJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// pingCloud does a fast HEAD to /healthz to populate the "Cloud connected" pill.
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
