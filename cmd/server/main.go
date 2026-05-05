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
	"github.com/pionerus/freefall/internal/billing"
	"github.com/pionerus/freefall/internal/branding"
	"github.com/pionerus/freefall/internal/clients"
	"github.com/pionerus/freefall/internal/config"
	"github.com/pionerus/freefall/internal/db"
	"github.com/pionerus/freefall/internal/drive"
	"github.com/pionerus/freefall/internal/email"
	"github.com/pionerus/freefall/internal/jump"
	"github.com/pionerus/freefall/internal/music"
	"github.com/pionerus/freefall/internal/operators"
	"github.com/pionerus/freefall/internal/platform"
	"github.com/pionerus/freefall/internal/secrets"
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
	// License-token middleware — kept for the legacy /api/v1/license/validate
	// endpoint that older studio binaries may still hit. New /api/v1/* routes
	// use session-cookie auth via sessions.RequireSession.
	_ = auth.RequireLicenseToken(pool)

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
	operatorsH := &operators.Handlers{
		DB:        pool,
		Templates: templates.Templates,
	}

	// Drive integration — loads OAuth credentials from .env. Operator UI
	// shows a "not configured" banner if the env vars are blank.
	driveClient := drive.New(drive.Config{
		ClientID:     cfg.GoogleOAuthClientID,
		ClientSecret: cfg.GoogleOAuthClientSecret,
		RedirectURL:  cfg.GoogleOAuthRedirectURL,
		Scopes:       drive.DefaultScopes(),
	}, pool)
	driveH := &drive.Handlers{
		Client:    driveClient,
		Templates: templates.Templates,
	}
	// Master key for AES-GCM. Load once at boot — rotation requires a
	// restart, which is fine for a single-process deployment.
	driveMasterKey, err := secrets.LoadMasterKey(ctx, pool)
	if err != nil {
		log.Fatalf("load drive master key: %v", err)
	}
	artifactsH := &jump.ArtifactsHandlers{DB: pool, Storage: deliverStorage, DriveClient: driveClient}

	// Phase 13 — outbound deliverables email. SMTP host blank in dev means
	// MailHog at localhost:51025 (default). In prod we point at smtp.resend.com.
	emailSender := email.New(email.Config{
		Host:     cfg.SMTPHost,
		Port:     cfg.SMTPPort,
		Username: cfg.SMTPUsername,
		Password: cfg.SMTPPassword,
		From:     cfg.SMTPFrom,
	})
	emailH := &jump.EmailHandlers{
		DB:        pool,
		Sender:    emailSender,
		Templates: templates.Templates,
		BaseURL:   cfg.PublicBaseURL,
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
	r.With(sessions.RequireOwner).Put("/admin/clients/{id}", clientsH.Update)
	r.With(sessions.RequireOwner).Delete("/admin/clients/{id}", clientsH.Delete)
	r.With(sessions.RequireOwner).Put("/admin/clients/{id}/assign", clientsH.Assign)
	r.With(sessions.RequireOwner).Post("/admin/clients/import", clientsH.ImportCSV)

	r.With(sessions.RequireOwner).Get("/admin/operators", operatorsH.List)
	r.With(sessions.RequireOwner).Post("/admin/operators", operatorsH.Create)
	r.With(sessions.RequireOwner).Get("/admin/operators/json", operatorsH.ListJSON)
	r.With(sessions.RequireOwner).Delete("/admin/operators/{id}", operatorsH.Delete)

	r.With(sessions.RequireOwner).Get("/admin/billing", func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		ctx := req.Context()

		// Current-month bill + 12-month history so the operator can see
		// "what we owe right now" + "trend over the year".
		y, m := billing.CurrentMonth()
		current, _ := billing.Compute(ctx, pool, s.TenantID, y, m)

		var history []billing.Bill
		startMonth := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -11, 0)
		for i := 0; i < 12; i++ {
			t := startMonth.AddDate(0, i, 0)
			b, err := billing.Compute(ctx, pool, s.TenantID, t.Year(), t.Month())
			if err == nil && b != nil {
				history = append(history, *b)
			}
		}
		extra := map[string]any{
			"History": history,
		}
		if current != nil {
			extra["CurrentBill"] = *current
		}

		data := adminPageData(ctx, pool, s, "billing", extra)
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
	r.With(sessions.RequirePlatformAdmin).Put("/platform/clubs/{id}", platformH.UpdateClub)
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
	// /storage now ships a real Google Drive connect flow (see drive.Handlers).
	// /dashboard, /clients, /projects are real handlers below.
	_ = data_tenantName // keep helper available for future operator pages
	r.With(sessions.RequireSession).Get("/operator/",         operatorDashboardHandler(pool))
	r.With(sessions.RequireSession).Get("/operator/storage",  driveH.Page)
	r.With(sessions.RequireSession).Post("/operator/storage/test", func(w http.ResponseWriter, req *http.Request) {
		driveH.Test(w, req, driveMasterKey)
	})
	r.With(sessions.RequireSession).Post("/operator/storage/disconnect", func(w http.ResponseWriter, req *http.Request) {
		driveH.Disconnect(w, req, driveMasterKey)
	})

	// OAuth dance for Google Drive. Both routes are operator-scoped — the
	// callback needs an active session to know whose row to upsert.
	r.With(sessions.RequireSession).Get("/auth/google-drive/start", driveH.Start)
	r.With(sessions.RequireSession).Get("/auth/google-drive/callback", func(w http.ResponseWriter, req *http.Request) {
		driveH.Callback(w, req, driveMasterKey)
	})

	r.With(sessions.RequireSession).Get("/operator/clients",  operatorClientsHandler(pool))
	r.With(sessions.RequireSession).Get("/operator/projects", operatorProjectsHandler(pool))

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
	r.With(sessions.RequireSession).Post("/api/v1/jumps/register", jumpH.Register)
	r.With(sessions.RequireSession).Get("/api/v1/jumps/{id}", jumpH.GetByIDForStudio)
	r.With(sessions.RequireSession).Get("/api/v1/operator/clients", operatorClientsAPIHandler(pool))
	r.With(sessions.RequireSession).Put("/api/v1/jumps/{id}/music", jumpH.SetMusic)
	r.With(sessions.RequireSession).Get("/api/v1/music", musicH.StudioCatalog)
	r.With(sessions.RequireSession).Post("/api/v1/music/suggest", musicH.StudioSuggest)
	r.With(sessions.RequireSession).Get("/api/v1/music/{id}/file", musicH.StudioDownload)
	r.With(sessions.RequireSession).Get("/api/v1/tenant/branding", brandH.GetForStudio)
	r.With(sessions.RequireSession).Post("/api/v1/jumps/{id}/artifacts/upload-url", artifactsH.RequestUploadURL)
	r.With(sessions.RequireSession).Post("/api/v1/jumps/{id}/artifacts", artifactsH.RegisterArtifact)
	r.With(sessions.RequireSession).Get("/api/v1/jumps/{id}/drive-token", func(w http.ResponseWriter, req *http.Request) {
		driveH.UploadToken(w, req, driveMasterKey)
	})
	// Phase 13 — deliverables email. /api/v1 path is for studio (auto-send
	// after render); /admin and /operator paths are for the manual resend
	// button. All three are session-authed and tenant-scoped.
	r.With(sessions.RequireSession).Post("/api/v1/jumps/{id}/send-email", emailH.Send)
	r.With(sessions.RequireOwner).Post("/admin/jumps/{id}/resend-email", emailH.Resend)
	r.With(sessions.RequireSession).Post("/operator/jumps/{id}/resend-email", emailH.Resend)

	// Public client-facing watch page. No auth — access_code is the bearer.
	r.Get("/watch/{access_code}", watchH.Render)
	r.Post("/watch/{access_code}/download", watchH.TrackDownload)

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

// operatorClientsHandler renders /operator/clients — the camera operator's
// view of "clients I've been assigned to". Status comes from
// v_client_status (canonical 5-step lifecycle).
func operatorClientsHandler(pool *db.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()

		type row struct {
			ID           int64
			Name         string
			Email        string
			Phone        string
			AccessCode   string
			LatestJumpAt time.Time
			Status       string
			JumpCount    int
		}
		rows, err := pool.Query(ctx, `
			SELECT
				v.client_id, v.name, COALESCE(v.email, ''), COALESCE(v.phone, ''),
				v.access_code,
				COALESCE(v.jump_created_at, '0001-01-01'::timestamptz),
				v.status,
				COALESCE((SELECT COUNT(*) FROM jumps jj WHERE jj.client_id = v.client_id), 0)
			FROM v_client_status v
			WHERE v.tenant_id = $1 AND v.assigned_operator_id = $2
			ORDER BY COALESCE(v.jump_created_at, v.client_created_at) DESC
			LIMIT 200`,
			s.TenantID, s.OperatorID,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		out := make([]row, 0, 16)
		var pending int // clients with no jump yet (status new/assigned)
		for rows.Next() {
			var r row
			if err := rows.Scan(
				&r.ID, &r.Name, &r.Email, &r.Phone,
				&r.AccessCode,
				&r.LatestJumpAt, &r.Status,
				&r.JumpCount,
			); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out = append(out, r)
			if r.JumpCount == 0 {
				pending++
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = templates.Templates.ExecuteTemplate(w, "operator_clients.html", map[string]any{
			"Active":        "clients",
			"PageTitle":     "My clients",
			"PageSub":       fmt.Sprintf("%d assigned · %d still need a jump", len(out), pending),
			"OperatorEmail": s.OperatorEmail,
			"OperatorRole":  s.OperatorRole,
			"TenantName":    data_tenantName(ctx, pool, s.TenantID),
			"Clients":       out,
			"Pending":       pending,
		})
	}
}

// operatorDashboardHandler renders /operator/ — the operator's home page.
// Pulls live data instead of the old phase-9 placeholder so the operator
// can see at a glance: today's assigned clients, this week's jumps,
// pending uploads. Querying is best-effort — any failure degrades to
// zeros so a transient DB hiccup doesn't break the whole page.
func operatorDashboardHandler(pool *db.Pool) http.HandlerFunc {
	type queueRow struct {
		ID         int64
		Name       string
		Email      string
		Phone      string
		AccessCode string
		JumpCount  int
	}
	type recentJumpRow struct {
		AccessCode string
		ClientName string
		Status     string
		CreatedAt  time.Time
	}
	return func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()

		// KPIs.
		var (
			assignedTotal int
			assignedNoJumps int
			jumpsAllTime  int
			jumpsThisWeek int
			deliveredAllTime int
		)
		_ = pool.QueryRow(ctx, `
			SELECT
			  (SELECT COUNT(*) FROM clients
			    WHERE tenant_id = $1 AND assigned_operator_id = $2),
			  (SELECT COUNT(*) FROM clients c
			    WHERE c.tenant_id = $1 AND c.assigned_operator_id = $2
			      AND NOT EXISTS (SELECT 1 FROM jumps j WHERE j.client_id = c.id)),
			  (SELECT COUNT(*) FROM jumps
			    WHERE tenant_id = $1 AND operator_id = $2),
			  (SELECT COUNT(*) FROM jumps
			    WHERE tenant_id = $1 AND operator_id = $2
			      AND created_at >= NOW() - INTERVAL '7 days'),
			  (SELECT COUNT(*) FROM jumps
			    WHERE tenant_id = $1 AND operator_id = $2
			      AND status IN ('ready', 'delivered'))`,
			s.TenantID, s.OperatorID,
		).Scan(&assignedTotal, &assignedNoJumps, &jumpsAllTime, &jumpsThisWeek, &deliveredAllTime)

		// Today's queue: 5 most recent assigned clients without a jump.
		queue := make([]queueRow, 0, 5)
		qrows, qerr := pool.Query(ctx, `
			SELECT c.id, c.name, COALESCE(c.email,''), COALESCE(c.phone,''),
			       c.access_code,
			       COALESCE((SELECT COUNT(*) FROM jumps jj WHERE jj.client_id = c.id), 0)
			FROM clients c
			WHERE c.tenant_id = $1 AND c.assigned_operator_id = $2
			  AND NOT EXISTS (SELECT 1 FROM jumps j WHERE j.client_id = c.id)
			ORDER BY c.created_at DESC
			LIMIT 5`,
			s.TenantID, s.OperatorID,
		)
		if qerr == nil {
			defer qrows.Close()
			for qrows.Next() {
				var r queueRow
				if err := qrows.Scan(&r.ID, &r.Name, &r.Email, &r.Phone, &r.AccessCode, &r.JumpCount); err == nil {
					queue = append(queue, r)
				}
			}
		}

		// Recent jumps: latest 5 of mine, any status.
		recent := make([]recentJumpRow, 0, 5)
		jrows, jerr := pool.Query(ctx, `
			SELECT c.access_code, c.name, j.status, j.created_at
			FROM jumps j JOIN clients c ON c.id = j.client_id
			WHERE j.tenant_id = $1 AND j.operator_id = $2
			ORDER BY j.created_at DESC
			LIMIT 5`,
			s.TenantID, s.OperatorID,
		)
		if jerr == nil {
			defer jrows.Close()
			for jrows.Next() {
				var r recentJumpRow
				if err := jrows.Scan(&r.AccessCode, &r.ClientName, &r.Status, &r.CreatedAt); err == nil {
					recent = append(recent, r)
				}
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = templates.Templates.ExecuteTemplate(w, "operator_dashboard.html", map[string]any{
			"Active":        "dashboard",
			"PageTitle":     "Welcome back",
			"PageSub":       fmt.Sprintf("%d assigned · %d this week · %d delivered", assignedTotal, jumpsThisWeek, deliveredAllTime),
			"OperatorEmail": s.OperatorEmail,
			"OperatorRole":  s.OperatorRole,
			"TenantName":    data_tenantName(ctx, pool, s.TenantID),
			"KPI": map[string]int{
				"AssignedTotal":   assignedTotal,
				"AssignedNoJumps": assignedNoJumps,
				"JumpsThisWeek":   jumpsThisWeek,
				"JumpsAllTime":    jumpsAllTime,
				"Delivered":       deliveredAllTime,
			},
			"Queue":  queue,
			"Recent": recent,
		})
	}
}

// operatorProjectsHandler renders /operator/projects — every jump where
// this operator is `jumps.operator_id`. Studio clip data lives in studio's
// SQLite (per-machine), not here, so we surface what cloud knows: status,
// access_code, client name, watch link, video size.
func operatorProjectsHandler(pool *db.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()

		type row struct {
			JumpID       int64
			AccessCode   string
			ClientName   string
			Status       string
			CreatedAt    time.Time
			HasArtifact  bool
			ArtifactSize int64
		}
		rows, err := pool.Query(ctx, `
			SELECT
				j.id, c.access_code, c.name,
				j.status, j.created_at,
				art.id IS NOT NULL  AS has_art,
				COALESCE(art.size_bytes, 0) AS art_size
			FROM jumps j
			JOIN clients c ON c.id = j.client_id
			LEFT JOIN LATERAL (
				SELECT id, size_bytes FROM jump_artifacts a
				WHERE a.jump_id = j.id AND a.kind = 'horizontal_1080p'
				ORDER BY uploaded_at DESC LIMIT 1
			) art ON true
			WHERE j.tenant_id = $1 AND j.operator_id = $2
			ORDER BY j.created_at DESC
			LIMIT 200`,
			s.TenantID, s.OperatorID,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		out := make([]row, 0, 16)
		statusN := map[string]int{}
		for rows.Next() {
			var r row
			if err := rows.Scan(
				&r.JumpID, &r.AccessCode, &r.ClientName,
				&r.Status, &r.CreatedAt,
				&r.HasArtifact, &r.ArtifactSize,
			); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out = append(out, r)
			statusN[r.Status]++
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = templates.Templates.ExecuteTemplate(w, "operator_projects.html", map[string]any{
			"Active":        "projects",
			"PageTitle":     "My projects",
			"PageSub":       fmt.Sprintf("%d jumps you've registered", len(out)),
			"OperatorEmail": s.OperatorEmail,
			"OperatorRole":  s.OperatorRole,
			"TenantName":    data_tenantName(ctx, pool, s.TenantID),
			"Jumps":         out,
			"StatusBreak":   statusN,
		})
	}
}

// operatorClientsAPIHandler returns clients assigned to the currently
// signed-in operator as JSON. Used by studio.exe to populate the
// "pick a client" dropdown on the new-project flow. The `status` field
// follows the canonical 5-step lifecycle from v_client_status:
//   new → assigned → in_progress → sent → downloaded
func operatorClientsAPIHandler(pool *db.Pool) http.HandlerFunc {
	type clientRow struct {
		ID           int64     `json:"id"`
		Name         string    `json:"name"`
		Email        string    `json:"email,omitempty"`
		Phone        string    `json:"phone,omitempty"`
		AccessCode   string    `json:"access_code"`
		LatestJumpAt time.Time `json:"latest_jump_at,omitempty"`
		Status       string    `json:"status"` // canonical 5-step
		JumpCount    int       `json:"jump_count"`
	}
	type response struct {
		Clients []clientRow `json:"clients"`
	}
	return func(w http.ResponseWriter, req *http.Request) {
		s := auth.MustFromContext(req.Context())
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		rows, err := pool.Query(ctx, `
			SELECT
				v.client_id, v.name, COALESCE(v.email, ''), COALESCE(v.phone, ''),
				v.access_code,
				COALESCE(v.jump_created_at, '0001-01-01'::timestamptz),
				v.status,
				COALESCE((SELECT COUNT(*) FROM jumps jj WHERE jj.client_id = v.client_id), 0)
			FROM v_client_status v
			WHERE v.tenant_id = $1 AND v.assigned_operator_id = $2
			ORDER BY COALESCE(v.jump_created_at, v.client_created_at) DESC
			LIMIT 200`,
			s.TenantID, s.OperatorID,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := make([]clientRow, 0, 16)
		for rows.Next() {
			var r clientRow
			if err := rows.Scan(
				&r.ID, &r.Name, &r.Email, &r.Phone,
				&r.AccessCode, &r.LatestJumpAt, &r.Status, &r.JumpCount,
			); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out = append(out, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{Clients: out})
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
//
// PlanLabel is gone — there are no plans, only per-jump rates. The rail now
// surfaces the current month's bill amount instead, computed via the
// billing package.
func adminPageData(ctx context.Context, pool *db.Pool, s auth.SessionData, active string, extra map[string]any) map[string]any {
	var name string
	_ = pool.QueryRow(ctx,
		`SELECT name FROM tenants WHERE id = $1`,
		s.TenantID,
	).Scan(&name)
	if name == "" {
		name = "Tenant"
	}

	// Compute current-month bill — best-effort; rail still renders if billing
	// query errors (e.g. tenant deleted but stale session). "—" sentinel keeps
	// the layout stable when there's nothing to bill yet.
	billLabel := "€0.00 this month"
	{
		y, m := billing.CurrentMonth()
		if b, berr := billing.Compute(ctx, pool, s.TenantID, y, m); berr == nil && b != nil {
			billLabel = "€" + b.EuroTotal() + " this month"
		}
	}

	data := map[string]any{
		"Active":         active,
		"OperatorEmail":  s.OperatorEmail,
		"OperatorRole":   s.OperatorRole,
		"TenantName":     name,
		"TenantInitials": tenantInitials(name),
		"PlanLabel":      billLabel, // legacy template field — now shows the bill
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
