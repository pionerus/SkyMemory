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
	"github.com/pionerus/freefall/internal/branding"
	"github.com/pionerus/freefall/internal/clients"
	"github.com/pionerus/freefall/internal/config"
	"github.com/pionerus/freefall/internal/db"
	"github.com/pionerus/freefall/internal/jump"
	"github.com/pionerus/freefall/internal/music"
	"github.com/pionerus/freefall/internal/platform"
	"github.com/pionerus/freefall/internal/storage"
	"github.com/pionerus/freefall/internal/watch"
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
	if len(os.Args) > 1 && os.Args[1] == "platform-admin" {
		if err := runPlatformAdmin(cfg.DatabaseURL, os.Args[2:]); err != nil {
			log.Fatalf("platform-admin: %v", err)
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

	// Branding storage — same MinIO/S3 endpoint, separate bucket. Holds
	// per-tenant watermark PNG + optional intro/outro clips.
	brandStorage, err := storage.NewBrandingClient(cfg)
	if err != nil {
		log.Fatalf("branding storage: %v", err)
	}
	brandBucketCtx, brandBucketCancel := context.WithTimeout(ctx, 8*time.Second)
	if berr := brandStorage.EnsureBucket(brandBucketCtx); berr != nil {
		log.Printf("WARN: branding bucket %q not ready (%v) — admin uploads will fail until it's reachable", cfg.BrandingBucket, berr)
	} else {
		log.Printf("branding bucket: %s @ %s", cfg.BrandingBucket, cfg.MusicEndpoint)
	}
	brandBucketCancel()
	brandH := &branding.Handlers{DB: pool, Storage: brandStorage}

	// Deliverables storage — final rendered videos uploaded by studio.exe
	// after each render (Phase 7.1). Same MinIO endpoint, separate bucket.
	deliverStorage, err := storage.NewDeliverablesClient(cfg)
	if err != nil {
		log.Fatalf("deliverables storage: %v", err)
	}
	delivBucketCtx, delivBucketCancel := context.WithTimeout(ctx, 8*time.Second)
	if berr := deliverStorage.EnsureBucket(delivBucketCtx); berr != nil {
		log.Printf("WARN: deliverables bucket %q not ready (%v) — studio uploads will fail until it's reachable", cfg.DeliverablesBucket, berr)
	} else {
		log.Printf("deliverables bucket: %s @ %s", cfg.DeliverablesBucket, cfg.MusicEndpoint)
	}
	delivBucketCancel()
	artifactsH := &jump.ArtifactsHandlers{DB: pool, Storage: deliverStorage}
	watchH := &watch.Handlers{
		DB:             pool,
		DeliverStorage: deliverStorage,
		Templates:      templates.Templates,
	}
	platformH := &platform.Handlers{
		DB:        pool,
		Templates: templates.Templates,
		BaseURL:   cfg.PublicBaseURL,
	}
	clientsH := &clients.Handlers{
		DB:        pool,
		Templates: templates.Templates,
	}

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	// Skydive Memory design assets — CSS + brand mark — embedded into the binary.
	r.Handle("/static/*", http.StripPrefix("/static/", templates.StaticHandler()))

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

	// Root: Skydive Memory dashboard for authenticated owners; redirect to /login
	// for anon, /operator/ for non-owner operators. Manual checks rather than
	// RequireSession middleware so unauthenticated hits get a friendly redirect
	// instead of a 401 JSON.
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		s := sessions.Read(req)
		if s.IsPlatformAdmin() {
			http.Redirect(w, req, "/platform/", http.StatusFound)
			return
		}
		if !s.IsAuthenticated() {
			http.Redirect(w, req, "/login", http.StatusFound)
			return
		}
		if !s.IsOwner() {
			http.Redirect(w, req, "/operator/", http.StatusFound)
			return
		}
		data := adminPageData(req.Context(), pool, s, "dash", nil)
		// Stat placeholders — Phase D (dashboard polish) will compute these from
		// jumps / watch_events / photo_orders / monthly_invoices.
		data["Stats"] = map[string]any{
			"JumpsRendered": 0, "JumpsTrend": "0%",
			"WatchClicks": 0, "WatchTrend": "0%",
			"PhotosSold": 0, "PhotosTrend": "0%",
			"MRR": "€0", "MRRTrend": "0%",
		}
		data["RecentJumps"] = []map[string]any{}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Templates.ExecuteTemplate(w, "dashboard.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Branding (Phase 6) — watermark PNG + size/opacity sliders + intro/outro
	// clips. Settings persist on tenants table; binary assets land in MinIO.
	r.With(sessions.RequireOwner).Get("/admin/branding", func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		data := adminPageData(req.Context(), pool, s, "brand", nil)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Templates.ExecuteTemplate(w, "admin_branding.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	r.With(sessions.RequireOwner).Get("/admin/branding/json", brandH.Get)
	r.With(sessions.RequireOwner).Put("/admin/branding/json", brandH.UpdateSettings)
	r.With(sessions.RequireOwner).Post("/admin/branding/watermark", brandH.UploadWatermark)
	r.With(sessions.RequireOwner).Delete("/admin/branding/watermark", brandH.DeleteWatermark)
	r.With(sessions.RequireOwner).Post("/admin/branding/intro", brandH.UploadIntro)
	r.With(sessions.RequireOwner).Delete("/admin/branding/intro", brandH.DeleteIntro)
	r.With(sessions.RequireOwner).Post("/admin/branding/outro", brandH.UploadOutro)
	r.With(sessions.RequireOwner).Delete("/admin/branding/outro", brandH.DeleteOutro)
	r.With(sessions.RequireOwner).Get("/admin/clients", clientsH.List)
	r.With(sessions.RequireOwner).Post("/admin/clients", clientsH.Create)

	r.With(sessions.RequireOwner).Get("/admin/billing", func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		data := adminPageData(req.Context(), pool, s, "billing", map[string]any{
			"UsageCount": 0, "UsageCap": 100, "UsagePct": 0,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Templates.ExecuteTemplate(w, "admin_billing.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	r.With(sessions.RequireSession).Get("/admin/settings", func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		data := adminPageData(req.Context(), pool, s, "settings", map[string]any{
			"OperatorRole": s.OperatorRole,
			"TenantSlug":   data_tenantSlug(req.Context(), pool, s.TenantID),
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Templates.ExecuteTemplate(w, "admin_settings.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Public auth pages (HTML)
	r.Get("/signup", renderTemplate("signup.html"))
	r.Get("/login", renderTemplate("login.html"))

	// Auth API
	r.Post("/auth/signup", authH.Signup)
	r.Post("/auth/login", authH.Login)
	r.Post("/auth/logout", authH.Logout)
	r.With(sessions.RequireSession).Get("/auth/me", authH.Me)

	// === Platform admin portal (cross-tenant ops, pricing, billing) ===
	r.Get("/platform/login", renderTemplate("platform_login.html"))
	r.Post("/platform/login", authH.PlatformLogin)
	r.Post("/platform/logout", authH.PlatformLogout)

	r.With(sessions.RequirePlatformAdmin).Get("/platform/clubs", platformH.ClubsList)
	r.With(sessions.RequirePlatformAdmin).Get("/platform/clubs/{id}", platformH.ClubDetail)
	r.With(sessions.RequirePlatformAdmin).Post("/platform/clubs", platformH.CreateClub)
	r.With(sessions.RequirePlatformAdmin).Get("/platform/operators", platformH.Operators)
	r.With(sessions.RequirePlatformAdmin).Get("/platform/jumps", platformH.Jumps)
	r.With(sessions.RequirePlatformAdmin).Get("/platform/watch-links", platformH.WatchLinks)
	r.With(sessions.RequirePlatformAdmin).Get("/platform/billing", platformH.Billing)
	r.With(sessions.RequirePlatformAdmin).Get("/platform/settings", platformH.Settings)

	r.With(sessions.RequirePlatformAdmin).Get("/platform/", func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		// Stats placeholders — Phase 10 wires real queries.
		stats := platformStats(req.Context(), pool)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = templates.Templates.ExecuteTemplate(w, "platform_dashboard.html", map[string]any{
			"AdminName": s.PlatformAdminName,
			"Stats":     stats,
		})
	})

	// === Operator portal (web dashboard for camera operators, in addition to studio.exe) ===
	// Operator portal pages. All four sections share operator_dashboard.html
	// with different `Active` + page titles — content blocks are scoped via
	// {{if eq .Active "..."}} so the rail highlights correctly. Real impls
	// land in Phase 9.x; for now they're informative placeholders.
	renderOperatorPage := func(active, title, sub string) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			s := auth.MustFromContext(req.Context())
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = templates.Templates.ExecuteTemplate(w, "operator_dashboard.html", map[string]any{
				"Active":        active,
				"PageTitle":     title,
				"PageSub":       sub,
				"OperatorEmail": s.OperatorEmail,
				"OperatorRole":  s.OperatorRole,
				"TenantName":    data_tenantName(req.Context(), pool, s.TenantID),
			})
		}
	}
	r.With(sessions.RequireSession).Get("/operator/",         renderOperatorPage("dashboard", "Operator", "My recent jumps · clients · storage"))
	r.With(sessions.RequireSession).Get("/operator/projects", renderOperatorPage("projects",  "My projects", "Web mirror of your studio.exe project list"))
	r.With(sessions.RequireSession).Get("/operator/clients",  renderOperatorPage("clients",   "My clients",  "Jumpers you have filmed"))
	r.With(sessions.RequireSession).Get("/operator/storage",  renderOperatorPage("storage",   "My storage",  "Personal cloud storage for clips + outputs"))

	// Admin: license token CRUD (owner-only). Tokens get installed in studio.exe.
	r.With(sessions.RequireOwner).Post("/admin/license-tokens", authH.CreateToken)
	r.With(sessions.RequireOwner).Get("/admin/license-tokens", authH.ListTokens)
	r.With(sessions.RequireOwner).Delete("/admin/license-tokens/{id}", authH.RevokeToken)

	// Admin: music library (owner-only). Tracks are GLOBAL — visible to all clubs.
	r.With(sessions.RequireOwner).Post("/admin/music", musicH.Upload)
	r.With(sessions.RequireOwner).Get("/admin/music", musicH.List)
	r.With(sessions.RequireOwner).Delete("/admin/music/{id}", musicH.Delete)

	// Admin HTML — music library page
	r.With(sessions.RequireOwner).Get("/admin/music-library", func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		data := adminPageData(req.Context(), pool, s, "music", nil)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Templates.ExecuteTemplate(w, "admin_music.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	// Admin HTML — owner-rendered page that drives the JSON CRUD above.
	r.With(sessions.RequireOwner).Get("/admin/tokens", func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		data := adminPageData(req.Context(), pool, s, "ops", map[string]any{
			"TenantSlug": data_tenantSlug(req.Context(), pool, s.TenantID),
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.Templates.ExecuteTemplate(w, "admin_tokens.html", data); err != nil {
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
	r.With(requireToken).Get("/api/v1/music/{id}/file", musicH.StudioDownload)
	r.With(requireToken).Get("/api/v1/tenant/branding", brandH.GetForStudio)
	r.With(requireToken).Post("/api/v1/jumps/{id}/artifacts/upload-url", artifactsH.RequestUploadURL)
	r.With(requireToken).Post("/api/v1/jumps/{id}/artifacts", artifactsH.RegisterArtifact)

	// Public client-facing watch page. No auth — access_code is the bearer.
	r.Get("/watch/{access_code}", watchH.Render)

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

// adminPageData builds the data map every admin template expects via the
// shared `admin-rail` partial: who's signed in, which tenant, which nav item
// to highlight. Page-specific extras get merged on top.
func adminPageData(ctx context.Context, pool *db.Pool, s auth.SessionData, active string, extra map[string]any) map[string]any {
	var name string
	var isFreeForever bool
	_ = pool.QueryRow(ctx,
		`SELECT name, is_free_forever FROM tenants WHERE id = $1`,
		s.TenantID,
	).Scan(&name, &isFreeForever)
	if name == "" {
		name = "Tenant"
	}

	data := map[string]any{
		"Active":         active,
		"OperatorEmail":  s.OperatorEmail,
		"OperatorRole":   s.OperatorRole,
		"TenantName":     name,
		"TenantInitials": tenantInitials(name),
		"PlanLabel":      planLabel(isFreeForever),
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

// data_tenantSlug pulls just the slug. Failures degrade silently to "" so a
// transient DB hiccup doesn't break the whole admin page render.
func data_tenantSlug(ctx context.Context, pool *db.Pool, tenantID int64) string {
	var slug string
	_ = pool.QueryRow(ctx, `SELECT slug FROM tenants WHERE id = $1`, tenantID).Scan(&slug)
	return slug
}

// data_tenantName looks up the human-readable tenant name for the operator
// portal header. Falls back to "Tenant" if the row is missing.
func data_tenantName(ctx context.Context, pool *db.Pool, tenantID int64) string {
	var name string
	_ = pool.QueryRow(ctx, `SELECT name FROM tenants WHERE id = $1`, tenantID).Scan(&name)
	if name == "" {
		return "Tenant"
	}
	return name
}

// platformStats computes the cross-tenant dashboard numbers. Phase 10 will
// add charts + per-club drill-ins; today we just count what we have so the
// stat cards show real values immediately.
type platformStatsView struct {
	Clubs          int
	Operators      int
	Jumps30d       int
	JumpsTotal     int
	WatchClicks30d int
}

func platformStats(ctx context.Context, pool *db.Pool) platformStatsView {
	var s platformStatsView
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE deleted_at IS NULL`).Scan(&s.Clubs)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`).Scan(&s.Operators)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM jumps`).Scan(&s.JumpsTotal)
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM jumps WHERE created_at > now() - INTERVAL '30 days'`,
	).Scan(&s.Jumps30d)
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM watch_events WHERE created_at > now() - INTERVAL '30 days'`,
	).Scan(&s.WatchClicks30d)
	return s
}

// tenantInitials picks up to 2 leading alphanumeric characters from a tenant
// name for the avatar tile in the rail. "Aero Club Ural" -> "AC".
func tenantInitials(name string) string {
	out := []rune{}
	prevWasSep := true
	for _, r := range name {
		if r == ' ' || r == '-' || r == '_' {
			prevWasSep = true
			continue
		}
		if prevWasSep && len(out) < 2 {
			if r >= 'a' && r <= 'z' {
				r = r - 'a' + 'A'
			}
			out = append(out, r)
		}
		prevWasSep = false
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// planLabel returns the plan name shown in the rail's bottom block. Until we
// have a richer plans table, the only signal is `tenants.is_free_forever`.
func planLabel(isFreeForever bool) string {
	if isFreeForever {
		return "Free"
	}
	return "Pro"
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

// runPlatformAdmin handles `server.exe platform-admin <subcommand>`.
//
//	add <email> <name>      create a platform admin (prompts for password)
//	list                    list platform admins
//	delete <email>          soft-delete a platform admin
//
// Platform admins are the YES/Skydive Memory employees who oversee all
// tenants. Distinct from `operators` which are tenant-scoped.
func runPlatformAdmin(databaseURL string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: server platform-admin <add|list|delete> [...]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("db open: %w", err)
	}
	defer pool.Close()

	switch args[0] {
	case "add":
		if len(args) < 3 {
			return fmt.Errorf("usage: server platform-admin add <email> <name>")
		}
		email, name := args[1], args[2]
		fmt.Print("Password: ")
		var pw string
		_, _ = fmt.Scanln(&pw)
		if pw == "" {
			return fmt.Errorf("password cannot be empty")
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return fmt.Errorf("hash password: %w", err)
		}
		var id int64
		err = pool.QueryRow(ctx,
			`INSERT INTO platform_admins (email, password_hash, name)
			 VALUES ($1, $2, $3) RETURNING id`,
			email, hash, name,
		).Scan(&id)
		if err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		log.Printf("platform-admin add: created id=%d email=%s name=%s", id, email, name)
		return nil

	case "list":
		rows, err := pool.Query(ctx, `
			SELECT id, email, name, last_login_at, created_at, deleted_at
			FROM platform_admins ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		fmt.Printf("%-4s %-32s %-20s %-12s\n", "ID", "EMAIL", "NAME", "STATUS")
		for rows.Next() {
			var (
				id            int64
				email, name   string
				lastLogin     *time.Time
				created       time.Time
				deleted       *time.Time
			)
			if err := rows.Scan(&id, &email, &name, &lastLogin, &created, &deleted); err != nil {
				return err
			}
			status := "active"
			if deleted != nil {
				status = "deleted"
			}
			fmt.Printf("%-4d %-32s %-20s %-12s\n", id, email, name, status)
		}
		return rows.Err()

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: server platform-admin delete <email>")
		}
		email := args[1]
		ct, err := pool.Exec(ctx,
			`UPDATE platform_admins SET deleted_at = now() WHERE email = $1 AND deleted_at IS NULL`,
			email,
		)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("no active platform admin with email %q", email)
		}
		log.Printf("platform-admin delete: soft-deleted email=%s", email)
		return nil

	default:
		return fmt.Errorf("unknown subcommand %q (expected add|list|delete)", args[0])
	}
}
