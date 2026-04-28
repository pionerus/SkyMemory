package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/pionerus/freefall/internal/auth"
	"github.com/pionerus/freefall/internal/config"
	"github.com/pionerus/freefall/internal/db"
	"github.com/pionerus/freefall/internal/jump"
	"github.com/pionerus/freefall/internal/music"
	"github.com/pionerus/freefall/internal/storage"
	"github.com/pionerus/freefall/web/server/templates"
)

func main() {
	cfg, err := config.LoadServer()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Sub-commands. `server.exe migrate up` etc. No args (or anything else) starts the HTTP server.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrate(cfg.DatabaseURL, os.Args[2:]); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	defer pool.Close()

	sessions := auth.NewManager(cfg.SecretKey, cfg.Env == "production")
	authH := &auth.Handlers{DB: pool, Sessions: sessions}
	jumpH := &jump.Handlers{DB: pool}
	requireToken := auth.RequireLicenseToken(pool)

	// Music storage. EnsureBucket is idempotent — safe on every boot.
	musicStorage, err := storage.NewMusicClient(cfg)
	if err != nil {
		log.Fatalf("music storage: %v", err)
	}
	bucketCtx, bucketCancel := context.WithTimeout(ctx, 8*time.Second)
	if berr := musicStorage.EnsureBucket(bucketCtx); berr != nil {
		log.Printf("WARN: music bucket %q not ready (%v) — admin uploads will fail until it's reachable", cfg.MusicBucket, berr)
	} else {
		log.Printf("music bucket: %s @ %s", cfg.MusicBucket, cfg.MusicEndpoint)
	}
	bucketCancel()
	musicH := &music.Handlers{DB: pool, Storage: musicStorage}

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"status": "ok",
			"env":    cfg.Env,
			"db":     "ok",
		}
		pingCtx, cncl := context.WithTimeout(r.Context(), 1*time.Second)
		defer cncl()
		if err := pool.Ping(pingCtx); err != nil {
			out["db"] = "down"
			out["status"] = "degraded"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	// Root: dashboard placeholder for authenticated users, redirect to /login otherwise.
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		s := sessions.Read(req)
		if !s.IsAuthenticated() {
			http.Redirect(w, req, "/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Freefall</title>
<style>body{font-family:system-ui;max-width:720px;margin:4rem auto;padding:0 1rem;color:#0a0a14}
a{color:#4f46e5}.card{background:white;border:1px solid #eee;border-radius:16px;padding:20px;margin:16px 0}
.btn{display:inline-block;padding:8px 14px;border-radius:8px;background:#4f46e5;color:white;text-decoration:none;font-weight:600;font-size:14px}
.btn.secondary{background:white;color:#0a0a14;border:1px solid #ddd}</style></head>
<body>
<h1>🪂 Welcome, %s</h1>
<p>Tenant ID: <code>%d</code> · role: <code>%s</code></p>

<div class="card">
  <h3 style="margin-top:0">Studio installs</h3>
  <p>Issue and revoke license tokens that operators paste into their studio.exe.</p>
  <p><a href="/admin/tokens" class="btn">Manage license tokens →</a></p>
</div>

<div class="card">
  <h3 style="margin-top:0">Music library</h3>
  <p>Upload royalty-free MP3 tracks. Studio operators pick from this catalog when finalising a project.</p>
  <p><a href="/admin/music-library" class="btn">Manage music library →</a></p>
</div>

<p style="margin-top:2rem;color:#888;font-size:13px">
  Diagnostics: <a href="/auth/me">/auth/me</a> · <a href="/healthz">/healthz</a>
</p>

<form method="POST" action="/auth/logout"><button type="submit" class="btn secondary">Sign out</button></form>
</body></html>`, s.OperatorEmail, s.TenantID, s.OperatorRole)
	})

	// Public auth pages (HTML)
	r.Get("/signup", renderTemplate("signup.html"))
	r.Get("/login", renderTemplate("login.html"))

	// Auth API
	r.Post("/auth/signup", authH.Signup)
	r.Post("/auth/login", authH.Login)
	r.Post("/auth/logout", authH.Logout)
	r.With(sessions.RequireSession).Get("/auth/me", authH.Me)

	// Admin: license token CRUD (owner-only). Tokens get installed in studio.exe.
	r.With(sessions.RequireOwner).Post("/admin/license-tokens", authH.CreateToken)
	r.With(sessions.RequireOwner).Get("/admin/license-tokens", authH.ListTokens)
	r.With(sessions.RequireOwner).Delete("/admin/license-tokens/{id}", authH.RevokeToken)

	// Admin: music library (owner-only). Tracks are GLOBAL — visible to all clubs.
	r.With(sessions.RequireOwner).Post("/admin/music", musicH.Upload)
	r.With(sessions.RequireOwner).Get("/admin/music", musicH.List)
	r.With(sessions.RequireOwner).Delete("/admin/music/{id}", musicH.Delete)

	// Admin HTML — music library page
	r.With(sessions.RequireOwner).Get("/admin/music-library", func(w http.ResponseWriter, r *http.Request) {
		s := auth.MustFromContext(r.Context())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Templates.ExecuteTemplate(w, "admin_music.html", map[string]any{
			"OperatorEmail": s.OperatorEmail,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Admin HTML — owner-rendered page that drives the JSON CRUD above.
	r.With(sessions.RequireOwner).Get("/admin/tokens", func(w http.ResponseWriter, r *http.Request) {
		s := auth.MustFromContext(r.Context())

		// Pull tenant slug for the page header. Failure is non-fatal — UI just shows id.
		var tenantSlug string
		_ = pool.QueryRow(r.Context(),
			`SELECT slug FROM tenants WHERE id = $1`, s.TenantID,
		).Scan(&tenantSlug)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		err := templates.Templates.ExecuteTemplate(w, "admin_tokens.html", map[string]any{
			"OperatorEmail": s.OperatorEmail,
			"TenantSlug":    tenantSlug,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// API v1: license validation (auth = the token in the request body itself, no session)
	r.Post("/api/v1/license/validate", authH.ValidateLicense)

	// API v1 — studio-facing endpoints. Each is gated by RequireLicenseToken.
	r.With(requireToken).Post("/api/v1/jumps/register", jumpH.Register)
	r.With(requireToken).Get("/api/v1/jumps/{id}", jumpH.GetByIDForStudio)
	r.With(requireToken).Put("/api/v1/jumps/{id}/music", jumpH.SetMusic)
	r.With(requireToken).Get("/api/v1/music", musicH.StudioCatalog)
	r.With(requireToken).Post("/api/v1/music/suggest", musicH.StudioSuggest)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("freefall-server listening on %s (env=%s)", cfg.HTTPAddr, cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cncl := context.WithTimeout(context.Background(), 10*time.Second)
	defer cncl()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// renderTemplate returns a chi http.HandlerFunc that renders an embedded
// HTML template by filename. Used for static-ish public pages (signup, login).
// Pages requiring per-request data should call templates.Templates directly.
func renderTemplate(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Templates.ExecuteTemplate(w, name, nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// runMigrate handles `server.exe migrate <subcommand>` invocations.
//   migrate up               apply all pending migrations
//   migrate down             revert last migration step
//   migrate version          print current version + dirty flag
//   migrate force <version>  force-set state (recovery only)
func runMigrate(databaseURL string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: server migrate <up|down|version|force VERSION>")
	}
	switch args[0] {
	case "up":
		if err := db.MigrateUp(databaseURL); err != nil {
			return err
		}
		v, dirty, err := db.MigrateVersion(databaseURL)
		if err != nil {
			return err
		}
		log.Printf("migrate up: now at version=%d dirty=%v", v, dirty)
		return nil
	case "down":
		if err := db.MigrateDown(databaseURL); err != nil {
			return err
		}
		v, dirty, err := db.MigrateVersion(databaseURL)
		if err != nil {
			return err
		}
		log.Printf("migrate down: now at version=%d dirty=%v", v, dirty)
		return nil
	case "version":
		v, dirty, err := db.MigrateVersion(databaseURL)
		if err != nil {
			return err
		}
		log.Printf("migrate version: %d dirty=%v", v, dirty)
		return nil
	case "force":
		if len(args) < 2 {
			return fmt.Errorf("usage: server migrate force <version>")
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("force: version must be an integer: %w", err)
		}
		if err := db.MigrateForce(databaseURL, n); err != nil {
			return err
		}
		log.Printf("migrate force: state set to version=%d dirty=false", n)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (expected up|down|version|force)", args[0])
	}
}
