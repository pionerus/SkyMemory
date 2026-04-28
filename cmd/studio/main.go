package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	v1 "github.com/pionerus/freefall/internal/api/v1"
	"github.com/pionerus/freefall/internal/config"
	"github.com/pionerus/freefall/internal/studio/ffprobe"
	"github.com/pionerus/freefall/internal/studio/jump"
	"github.com/pionerus/freefall/internal/studio/license"
	"github.com/pionerus/freefall/internal/studio/state"
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

	stateDB, err := state.Open(ctx, cfg.StatePath)
	if err != nil {
		log.Fatalf("state.Open: %v", err)
	}
	defer stateDB.Close()
	log.Printf("state: %s", stateDB.Path())

	// Job dir = sibling of state.db. Holds uploaded clips (and later, intermediate
	// renders, music cache, output MP4s). Created lazily on first upload.
	jobsDir := filepath.Join(filepath.Dir(cfg.StatePath), "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		log.Fatalf("mkdir jobs dir: %v", err)
	}
	log.Printf("jobs: %s", jobsDir)

	if !ffprobe.IsAvailable() {
		log.Printf("WARN: ffprobe not on PATH — clip uploads will be accepted without metadata. Install ffmpeg.")
	}

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

	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		res, lastAt := licenseMgr.Snapshot()

		// Pull projects list — best-effort. If state is unreadable we still render the page.
		projects, perr := stateDB.ListProjects(req.Context(), false)
		if perr != nil {
			log.Printf("WARN: list projects: %v", perr)
		}

		data := homeData{
			Version:          version,
			Platform:         runtime.GOOS + "/" + runtime.GOARCH,
			Addr:             cfg.HTTPAddr,
			CloudBaseURL:     cfg.CloudBaseURL,
			StatePath:        cfg.StatePath,
			TokenConfigured:  cfg.LicenseToken != "",
			License:          res,
			LicenseCheckedAt: lastAt,
			CloudReachable:   pingCloud(cfg.CloudBaseURL),
			Projects:         projects,
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

		// Persist locally so the project survives studio restarts.
		localID, err := stateDB.CreateProject(callCtx, state.Project{
			RemoteJumpID:      out.JumpID,
			RemoteClientID:    out.ClientID,
			AccessCode:        out.AccessCode,
			ClientName:        jr.ClientName,
			ClientEmail:       jr.ClientEmail,
			ClientPhone:       jr.ClientPhone,
			Output1080p:       jr.Output1080p,
			Output4K:          jr.Output4K,
			OutputVertical:    jr.OutputVertical,
			OutputPhotos:      jr.OutputPhotos,
			HasOperatorPhotos: jr.HasOperatorUploadedPhotos,
		})
		if err != nil {
			// Cloud succeeded; local save failed. Don't fail the request — we have an
			// access_code to show. Surface a warning so the operator sees the gap.
			log.Printf("WARN: cloud register OK but local persist failed: %v", err)
			localID = 0
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = ui.Templates.ExecuteTemplate(w, "new_project_done.html", map[string]any{
			"License":                   res,
			"CloudBaseURL":              cfg.CloudBaseURL,
			"LocalID":                   localID,
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

	// Detail page for one local project. Shows real clip slots driven by state.db.
	r.Get("/projects/{id}", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, err := stateDB.GetProject(req.Context(), id)
		if errors.Is(err, state.ErrNotFound) {
			http.NotFound(w, req)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		clips, err := stateDB.ListClips(req.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Build a {kind -> *Clip} map for template-side lookup per slot.
		clipByKind := map[string]*state.Clip{}
		for i := range clips {
			c := clips[i]
			clipByKind[c.Kind] = &c
		}

		licRes, _ := licenseMgr.Snapshot()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = ui.Templates.ExecuteTemplate(w, "project_detail.html", map[string]any{
			"License":             licRes,
			"CloudBaseURL":        cfg.CloudBaseURL,
			"P":                   p,
			"AccessCodeCanonical": strings.ReplaceAll(p.AccessCode, "-", ""),
			"CanonicalKinds":      state.CanonicalKinds(),
			"ClipByKind":          clipByKind,
			"FFprobeAvailable":    ffprobe.IsAvailable(),
		})
	})

	// POST a clip file into a project's slot. Multipart with field name "file".
	// Stores under <jobsDir>/<project_id>/<sanitized_kind>.<ext>, runs ffprobe,
	// upserts a row in clips. Returns the resulting clip JSON.
	r.Post("/projects/{id}/clips/{kind}", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		kind := chi.URLParam(req, "kind")
		if !isValidKind(kind) {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_KIND", "message": "Unknown segment kind: " + kind})
			return
		}

		// Verify project exists locally (also enforces tenant isolation indirectly —
		// state.db is per-machine; nobody else can hit this endpoint).
		if _, err := stateDB.GetProject(req.Context(), id); err != nil {
			if errors.Is(err, state.ErrNotFound) {
				writeStudioJSON(w, http.StatusNotFound, map[string]string{"code": "PROJECT_NOT_FOUND", "message": "Project not found in local state.db."})
				return
			}
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}

		// Cap upload size at 30GB. 4K originals can be huge but we don't want a runaway disk fill.
		if err := req.ParseMultipartForm(64 << 20); err != nil { // 64MB in-memory threshold; rest streams to temp
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_FORM", "message": err.Error()})
			return
		}
		f, fh, err := req.FormFile("file")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "FILE_MISSING", "message": "Form field 'file' is missing."})
			return
		}
		defer f.Close()
		const maxClipBytes = int64(30) << 30 // 30GB
		if fh.Size > maxClipBytes {
			writeStudioJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"code": "FILE_TOO_LARGE", "message": "Clip larger than 30GB cap.",
			})
			return
		}

		ext := filepath.Ext(fh.Filename)
		if ext == "" {
			ext = ".mp4"
		}
		safeKind := sanitizeKindForFilename(kind)
		projectDir := filepath.Join(jobsDir, strconv.FormatInt(id, 10))
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "FS_ERROR", "message": err.Error()})
			return
		}
		dstPath := filepath.Join(projectDir, safeKind+ext)

		// If a previous file existed for this slot with a different extension, drop it.
		// Same-extension case is handled by the os.Create overwrite below.
		if oldClip, _ := stateDB.GetClip(req.Context(), id, kind); oldClip != nil && oldClip.SourcePath != "" && oldClip.SourcePath != dstPath {
			if filepath.Dir(oldClip.SourcePath) == projectDir {
				_ = os.Remove(oldClip.SourcePath)
			}
		}

		dst, err := os.Create(dstPath)
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "FS_ERROR", "message": err.Error()})
			return
		}
		written, err := io.Copy(dst, f)
		closeErr := dst.Close()
		if err != nil {
			_ = os.Remove(dstPath)
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "WRITE_ERROR", "message": err.Error()})
			return
		}
		if closeErr != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "WRITE_ERROR", "message": closeErr.Error()})
			return
		}

		// Best-effort ffprobe. If it fails (or ffprobe is missing) we still record the clip;
		// metadata will be empty and the UI surfaces a "Probe failed" hint.
		clipRow := state.Clip{
			ProjectID:       id,
			Kind:            kind,
			SourcePath:      dstPath,
			SourceFilename:  fh.Filename,
			SourceSizeBytes: written,
		}
		if md, err := ffprobe.Probe(req.Context(), dstPath); err == nil && md != nil {
			clipRow.DurationSeconds = md.DurationSeconds
			clipRow.Codec = md.Codec
			clipRow.Width = md.Width
			clipRow.Height = md.Height
			clipRow.FPS = md.FPS
			clipRow.HasAudio = md.HasAudio
			clipRow.AudioCodec = md.AudioCodec
			// Default trim = full clip; operator narrows it on the trim panel.
			clipRow.TrimInSeconds = 0
			clipRow.TrimOutSeconds = md.DurationSeconds
		} else if err != nil {
			log.Printf("WARN: ffprobe failed on %s: %v", dstPath, err)
		}

		clipID, err := stateDB.UpsertClip(req.Context(), clipRow)
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		clipRow.ID = clipID
		writeStudioJSON(w, http.StatusOK, clipRow)
	})

	// DELETE a clip from a slot. Removes both the disk file (if under our jobs dir)
	// and the state.db row. Used by the "Replace" button in the UI.
	r.Delete("/projects/{id}/clips/{kind}", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		kind := chi.URLParam(req, "kind")
		clip, err := stateDB.GetClip(req.Context(), id, kind)
		if errors.Is(err, state.ErrNotFound) {
			writeStudioJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "message": "No clip in that slot."})
			return
		}
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}

		// Only delete the file if it's inside our jobs dir (defensive — never delete
		// a path the operator might have given us pointing elsewhere).
		if strings.HasPrefix(clip.SourcePath, jobsDir+string(os.PathSeparator)) {
			_ = os.Remove(clip.SourcePath)
		}

		if err := stateDB.DeleteClip(req.Context(), id, kind); err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		writeStudioJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})

	// PUT trim window. Body: {trim_in: 0, trim_out: 12.34, auto_suggested: false}.
	r.Put("/projects/{id}/clips/{kind}/trim", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		kind := chi.URLParam(req, "kind")

		var body struct {
			TrimIn        float64 `json:"trim_in"`
			TrimOut       float64 `json:"trim_out"`
			AutoSuggested bool    `json:"auto_suggested"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_JSON", "message": err.Error()})
			return
		}

		clip, err := stateDB.GetClip(req.Context(), id, kind)
		if errors.Is(err, state.ErrNotFound) {
			writeStudioJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "message": "No clip in that slot."})
			return
		}
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}

		// Validate. trim_out=0 is allowed and means "use full duration".
		if body.TrimIn < 0 {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_TRIM", "message": "trim_in must be ≥ 0"})
			return
		}
		if body.TrimOut > 0 && body.TrimOut <= body.TrimIn {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_TRIM", "message": "trim_out must be greater than trim_in"})
			return
		}
		if clip.DurationSeconds > 0 && body.TrimOut > clip.DurationSeconds+0.5 {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_TRIM", "message": "trim_out exceeds clip duration"})
			return
		}

		if err := stateDB.UpdateClipTrim(req.Context(), id, kind, body.TrimIn, body.TrimOut, body.AutoSuggested); err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		writeStudioJSON(w, http.StatusOK, map[string]any{
			"status":         "updated",
			"trim_in":        body.TrimIn,
			"trim_out":       body.TrimOut,
			"auto_suggested": body.AutoSuggested,
		})
	})

	// Stream the raw clip file (for inline <video> preview during trim). Uses
	// http.ServeFile which natively handles Range requests, so the browser can
	// scrub the timeline without downloading the whole thing.
	r.Get("/projects/{id}/clips/{kind}/file", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		kind := chi.URLParam(req, "kind")
		clip, err := stateDB.GetClip(req.Context(), id, kind)
		if errors.Is(err, state.ErrNotFound) {
			http.NotFound(w, req)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Defensive: only serve files we ourselves wrote (under jobsDir).
		if !strings.HasPrefix(clip.SourcePath, jobsDir+string(os.PathSeparator)) {
			http.Error(w, "clip path is outside jobs directory", http.StatusForbidden)
			return
		}
		http.ServeFile(w, req, clip.SourcePath)
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
	Projects         []state.Project
}

// writeStudioJSON sends a JSON response from a studio handler.
func writeStudioJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// parseInt64URLParam pulls a chi URL param and parses it as int64. Returns a
// friendly error so handlers can return 400.
func parseInt64URLParam(req *http.Request, name string) (int64, error) {
	s := chi.URLParam(req, name)
	if s == "" {
		return 0, errors.New(name + " is required")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errors.New(name + " must be an integer")
	}
	return n, nil
}

// isValidKind permits the 7 canonical segment kinds plus operator-defined custom
// ones with a "custom:" prefix and a non-empty alphanumeric label.
func isValidKind(k string) bool {
	switch k {
	case state.KindIntro, state.KindInterviewPre, state.KindWalk,
		state.KindInterviewPlane, state.KindFreefall, state.KindLanding, state.KindClosing:
		return true
	}
	if !strings.HasPrefix(k, state.CustomPrefix) {
		return false
	}
	label := k[len(state.CustomPrefix):]
	if label == "" || len(label) > 40 {
		return false
	}
	for _, c := range label {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// sanitizeKindForFilename converts a kind to a safe filesystem token.
// "interview_pre" -> "interview_pre", "custom:slow_motion" -> "custom_slow_motion".
func sanitizeKindForFilename(k string) string {
	return strings.ReplaceAll(k, ":", "_")
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
