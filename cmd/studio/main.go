package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	studiobranding "github.com/pionerus/freefall/internal/studio/branding"
	"github.com/pionerus/freefall/internal/studio/delivery"
	"github.com/pionerus/freefall/internal/studio/ffprobe"
	"github.com/pionerus/freefall/internal/studio/jump"
	"github.com/pionerus/freefall/internal/studio/license"
	studiomusic "github.com/pionerus/freefall/internal/studio/music"
	"github.com/pionerus/freefall/internal/studio/pipeline"
	"github.com/pionerus/freefall/internal/studio/state"
	"github.com/pionerus/freefall/internal/studio/trim"
	"github.com/pionerus/freefall/internal/studio/ui"
)

// version is overridden at build time via -ldflags "-X main.version=..."
var version = "0.0.1-dev"

// logFilePath holds the resolved on-disk path of studio.log so the /log
// endpoint can read it back. Set in main() after we know cfg.StatePath.
var logFilePath string

func main() {
	cfg, err := config.LoadStudio()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Persistent log — sits next to state.db so it follows the studio's
	// data dir. Append-mode: each studio session adds to the existing
	// file (capped naturally because we trim on read; not on disk for
	// now — that's a Phase 16 polish item if it ever matters).
	logFilePath = filepath.Join(filepath.Dir(cfg.StatePath), "studio.log")
	logF, lferr := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if lferr != nil {
		log.Printf("WARN: open log file %q: %v — logs will only go to stderr", logFilePath, lferr)
	} else {
		defer logF.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, logF))
		log.Printf("======== studio start v%s @ %s ========", version, time.Now().Format(time.RFC3339))
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
	musicClient := studiomusic.NewClient(cfg.CloudBaseURL, cfg.LicenseToken)

	// Music cache lives next to state.db so backups/cleanup are easy to reason about.
	musicCacheDir := filepath.Join(filepath.Dir(cfg.StatePath), "music-cache")
	musicCache, err := studiomusic.NewCache(musicCacheDir, musicClient)
	if err != nil {
		log.Fatalf("music cache: %v", err)
	}
	log.Printf("music cache: %s", musicCacheDir)

	// Branding cache (Phase 6.5) — same disk-cache pattern as music. Each
	// render fetches the tenant's bundle and re-downloads only when the
	// cloud-reported ETag has changed.
	brandingClient := studiobranding.NewClient(cfg.CloudBaseURL, cfg.LicenseToken)
	brandingCacheDir := filepath.Join(filepath.Dir(cfg.StatePath), "branding-cache")
	brandingCache, err := studiobranding.NewCache(brandingCacheDir, brandingClient)
	if err != nil {
		log.Fatalf("branding cache: %v", err)
	}
	log.Printf("branding cache: %s", brandingCacheDir)

	pipelineRunner := &pipeline.Runner{
		DB:               stateDB,
		JobsDir:          jobsDir,
		MusicCache:       musicCache,
		BrandingProvider: &brandingProvider{cache: brandingCache, license: licenseMgr},
	}

	// Delivery client (Phase 7.1) — uploads rendered MP4s to cloud after each
	// successful Run() so the watch page has something to play.
	deliveryClient := delivery.NewClient(cfg.CloudBaseURL, cfg.LicenseToken)
	runRegistry := &pipeline.RunRegistry{}

	// Recover from a previous crash / kill: any generation row left in an
	// in-progress status is bogus now (studio just restarted, no ffmpeg
	// running). Mark them failed so the UI doesn't spin forever.
	if n, err := stateDB.MarkStaleGenerationsFailed(context.Background()); err != nil {
		log.Printf("startup: mark stale generations: %v", err)
	} else if n > 0 {
		log.Printf("startup: marked %d stale generation(s) as failed", n)
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	// Skydive Memory design assets — CSS + brand mark — embedded into the binary.
	r.Handle("/static/*", http.StripPrefix("/static/", ui.StaticHandler()))

	// /log — last N lines of studio.log. Used by the generate-screen UI to
	// show ffmpeg stderr / pipeline messages when something hangs or fails
	// without a clean error popup. Plain text so a dev can also `curl` it.
	r.Get("/log", func(w http.ResponseWriter, req *http.Request) {
		nLines := 400
		if v := req.URL.Query().Get("lines"); v != "" {
			if n, perr := strconv.Atoi(v); perr == nil && n > 0 && n <= 5000 {
				nLines = n
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if logFilePath == "" {
			fmt.Fprintln(w, "(no log file — check stderr)")
			return
		}
		fmt.Fprintln(w, "# studio.log path:", logFilePath)
		fmt.Fprintln(w, "# tail:", nLines, "lines")
		fmt.Fprintln(w, "# ----------------------------------------")
		fmt.Fprintln(w, tailLog(logFilePath, nLines))
	})

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
			Port:             portFromAddr(cfg.HTTPAddr),
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

	// Settings dialog backing data — keyed off this so the studio modal can
	// fetch fresh state without a page reload.
	r.Get("/settings.json", func(w http.ResponseWriter, req *http.Request) {
		encoder := pipelineRunner.EncoderName(req.Context())
		encoderDetail := "Intel iGPU"
		if encoder == "CPU" {
			encoderDetail = "libx264 fallback"
		}
		writeStudioJSON(w, http.StatusOK, map[string]any{
			"encoder":         encoder,
			"encoder_detail":  encoderDetail,
			"jobs_dir":        jobsDir,
			"token_masked":    maskToken(cfg.LicenseToken),
			"version":         version,
			"platform":        runtime.GOOS + "/" + runtime.GOARCH,
			"cloud_url":       cfg.CloudBaseURL,
			"cloud_reachable": pingCloud(cfg.CloudBaseURL),
		})
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

	// Wizard step 2: clips + trim + music. Operator lands here right after
	// creating a project; "Continue to generate" leads to step 3.
	clipsHandler := func(w http.ResponseWriter, req *http.Request) {
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
		clipByKind := map[string]*state.Clip{}
		for i := range clips {
			c := clips[i]
			clipByKind[c.Kind] = &c
		}

		// Fetch all cut zones for this project's clips so the template can
		// render them as red striped overlays inside each trim slider.
		cutsByClipID, _ := stateDB.ListCutsForProject(req.Context(), id)
		cutsByKind := map[string][]state.Cut{}
		for kind, c := range clipByKind {
			cutsByKind[kind] = cutsByClipID[c.ID]
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
			"CutsByKind":          cutsByKind,
			"FFprobeAvailable":    ffprobe.IsAvailable(),
		})
	}
	// Canonical wizard URL.
	r.Get("/projects/{id}/clips", clipsHandler)
	// Backwards-compat: bare /projects/{id} redirects to /clips.
	r.Get("/projects/{id}", func(w http.ResponseWriter, req *http.Request) {
		id := chi.URLParam(req, "id")
		http.Redirect(w, req, "/projects/"+id+"/clips", http.StatusFound)
	})

	// Wizard step 3: final generate. Renders the generate panel only — no
	// clip board, no music picker. Operator clicks one big button, ffmpeg
	// runs, output is shown, email goes out (Phase 13). Failures send the
	// operator back to /clips to fix things.
	r.Get("/projects/{id}/generate", func(w http.ResponseWriter, req *http.Request) {
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
		clips, _ := stateDB.ListClips(req.Context(), id)
		licRes, _ := licenseMgr.Snapshot()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = ui.Templates.ExecuteTemplate(w, "project_generate.html", map[string]any{
			"License":             licRes,
			"CloudBaseURL":        cfg.CloudBaseURL,
			"P":                   p,
			"AccessCodeCanonical": strings.ReplaceAll(p.AccessCode, "-", ""),
			"ClipCount":           len(clips),
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

	// === Cut zones (Phase 3.3) ===
	// Operator paints exclusion bands inside the trim window — pipeline drops
	// those sub-ranges via split + concat in the filter graph.

	// GET cuts for a clip slot. Used after a save to refresh the UI without a
	// full page reload.
	r.Get("/projects/{id}/clips/{kind}/cuts", func(w http.ResponseWriter, req *http.Request) {
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
		cuts, err := stateDB.ListCuts(req.Context(), clip.ID)
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		writeStudioJSON(w, http.StatusOK, map[string]any{"cuts": cuts})
	})

	// POST a new cut. Body: {start, end, reason?}. Validation enforces
	// non-degenerate range and that the zone sits inside the trim window.
	r.Post("/projects/{id}/clips/{kind}/cuts", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		kind := chi.URLParam(req, "kind")
		var body struct {
			Start  float64 `json:"start"`
			End    float64 `json:"end"`
			Reason string  `json:"reason"`
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
		// Trim window bounds (effective trim_out = trim_out or duration).
		tIn := clip.TrimInSeconds
		if tIn < 0 {
			tIn = 0
		}
		tOut := clip.TrimOutSeconds
		if tOut <= 0 || tOut > clip.DurationSeconds {
			tOut = clip.DurationSeconds
		}
		if body.End <= body.Start {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_RANGE", "message": "end must be > start"})
			return
		}
		if body.Start < tIn-0.05 || body.End > tOut+0.05 {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{
				"code":    "OUT_OF_TRIM",
				"message": fmt.Sprintf("cut zone must sit inside the trim window [%.2f, %.2f]", tIn, tOut),
			})
			return
		}
		cutID, err := stateDB.CreateCut(req.Context(), clip.ID, body.Start, body.End, body.Reason, false)
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		writeStudioJSON(w, http.StatusOK, map[string]any{
			"id":     cutID,
			"start":  body.Start,
			"end":    body.End,
			"reason": body.Reason,
		})
	})

	// DELETE a cut by id. We resolve project + clip via JOIN so the response
	// can be useful for the UI even after the row is gone.
	r.Delete("/cuts/{id}", func(w http.ResponseWriter, req *http.Request) {
		cutID, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		// Validate the cut exists. (Empty result = already deleted = treat as 404.)
		_, _, err = stateDB.GetCutClip(req.Context(), cutID)
		if errors.Is(err, sql.ErrNoRows) {
			writeStudioJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "message": "Cut not found."})
			return
		}
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		if err := stateDB.DeleteCut(req.Context(), cutID); err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		writeStudioJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})

	// POST .../trim/auto — runs the per-kind heuristic (silencedetect for audio
	// kinds, positional rules for motion kinds) and returns a suggested
	// (trim_in, trim_out) without persisting. UI populates the sliders and shows
	// the reason; operator clicks Save to commit (which goes to PUT .../trim).
	r.Post("/projects/{id}/clips/{kind}/trim/auto", func(w http.ResponseWriter, req *http.Request) {
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
		if !ffprobe.IsAvailable() {
			writeStudioJSON(w, http.StatusServiceUnavailable, map[string]string{
				"code":    "FFMPEG_MISSING",
				"message": "Auto-trim needs ffmpeg on PATH. Install ffmpeg and restart studio.",
			})
			return
		}

		suggestion, err := trim.Suggest(req.Context(), clip)
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{
				"code": "AUTO_TRIM_FAILED", "message": err.Error(),
			})
			return
		}

		writeStudioJSON(w, http.StatusOK, map[string]any{
			"trim_in":  suggestion.TrimIn,
			"trim_out": suggestion.TrimOut,
			"reason":   suggestion.Reason,
		})
	})

	// GET music catalog — proxies to cloud /api/v1/music. Studio doesn't cache;
	// each render of project_detail re-fetches so presigned URLs are fresh.
	r.Get("/projects/{id}/music/catalog", func(w http.ResponseWriter, req *http.Request) {
		// project id is in the URL but the cloud catalog endpoint is project-agnostic;
		// we still parse it to keep the URL shape consistent with the rest of /projects/{id}/*.
		if _, err := parseInt64URLParam(req, "id"); err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		callCtx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
		defer cancel()
		out, err := musicClient.Catalog(callCtx)
		if err != nil {
			var apiErr *studiomusic.APIError
			if errors.As(err, &apiErr) {
				writeStudioJSON(w, apiErr.HTTPStatus, map[string]string{"code": apiErr.Code, "message": apiErr.Message})
				return
			}
			writeStudioJSON(w, http.StatusBadGateway, map[string]string{"code": "CLOUD_UNREACHABLE", "message": err.Error()})
			return
		}
		writeStudioJSON(w, http.StatusOK, out)
	})

	// GET music suggest — derives target duration from this project's clips,
	// then asks cloud for top-3 ranked picks. Studio doesn't cache; suggestions
	// reflect current trim state every time.
	r.Get("/projects/{id}/music/suggest", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		if _, err := stateDB.GetProject(req.Context(), id); err != nil {
			if errors.Is(err, state.ErrNotFound) {
				writeStudioJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "message": "Project not in local state.db."})
				return
			}
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		total, err := stateDB.SumProjectClipDuration(req.Context(), id)
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		// Optional ?mood=epic,fun query string for filter overrides.
		var moods []string
		if raw := req.URL.Query().Get("mood"); raw != "" {
			for _, m := range strings.Split(raw, ",") {
				m = strings.TrimSpace(m)
				if m != "" {
					moods = append(moods, m)
				}
			}
		}

		callCtx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
		defer cancel()
		out, err := musicClient.Suggest(callCtx, int(total+0.5), moods, 3)
		if err != nil {
			var apiErr *studiomusic.APIError
			if errors.As(err, &apiErr) {
				writeStudioJSON(w, apiErr.HTTPStatus, map[string]string{"code": apiErr.Code, "message": apiErr.Message})
				return
			}
			writeStudioJSON(w, http.StatusBadGateway, map[string]string{"code": "CLOUD_UNREACHABLE", "message": err.Error()})
			return
		}
		writeStudioJSON(w, http.StatusOK, map[string]any{
			"target_duration_seconds": int(total + 0.5),
			"mood":                    moods,
			"tracks":                  out.Tracks,
		})
	})

	// PUT music — body {music_track_id, music_title, music_artist, music_duration_s}.
	// The denormalised title/artist/duration come from the catalog row the operator
	// just clicked, so we don't need a second round-trip to read them.
	// Sends to cloud first; persists local SQLite snapshot only if cloud accepts.
	r.Put("/projects/{id}/music", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}

		var body struct {
			MusicTrackID    int64   `json:"music_track_id"`
			MusicTitle      string  `json:"music_title"`
			MusicArtist     string  `json:"music_artist"`
			MusicDurationS  float64 `json:"music_duration_s"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_JSON", "message": err.Error()})
			return
		}

		p, err := stateDB.GetProject(req.Context(), id)
		if errors.Is(err, state.ErrNotFound) {
			writeStudioJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "message": "Project not in local state.db."})
			return
		}
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}

		callCtx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
		defer cancel()
		if err := musicClient.SetJumpMusic(callCtx, p.RemoteJumpID, body.MusicTrackID); err != nil {
			var apiErr *studiomusic.APIError
			if errors.As(err, &apiErr) {
				writeStudioJSON(w, apiErr.HTTPStatus, map[string]string{"code": apiErr.Code, "message": apiErr.Message})
				return
			}
			writeStudioJSON(w, http.StatusBadGateway, map[string]string{"code": "CLOUD_UNREACHABLE", "message": err.Error()})
			return
		}

		if err := stateDB.SetProjectMusic(req.Context(), id, body.MusicTrackID, body.MusicTitle, body.MusicArtist, body.MusicDurationS); err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "LOCAL_PERSIST_FAILED", "message": err.Error()})
			return
		}
		writeStudioJSON(w, http.StatusOK, map[string]any{
			"status":           "updated",
			"music_track_id":   body.MusicTrackID,
			"music_title":      body.MusicTitle,
			"music_artist":     body.MusicArtist,
			"music_duration_s": body.MusicDurationS,
		})
	})

	// POST /projects/{id}/generate — kick off the FFmpeg pipeline.
	// Synchronous goroutine; UI polls /generations for status.
	r.Post("/projects/{id}/generate", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		if _, err := stateDB.GetProject(req.Context(), id); err != nil {
			if errors.Is(err, state.ErrNotFound) {
				writeStudioJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "message": "Project not in local state.db."})
				return
			}
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		if !ffprobe.IsAvailable() {
			writeStudioJSON(w, http.StatusServiceUnavailable, map[string]string{
				"code": "FFMPEG_MISSING", "message": "Pipeline needs ffmpeg on PATH. Install ffmpeg and restart studio.",
			})
			return
		}

		// QSV iGPU has one encoder engine — running two pipelines at once
		// dead-locks the driver. Reject early with a clear 409 instead.
		if runRegistry.IsBusy() {
			writeStudioJSON(w, http.StatusConflict, map[string]string{
				"code":    "ANOTHER_RUNNING",
				"message": "Another generation is already running. Wait for it to finish or click Stop on the running project.",
			})
			return
		}

		// Registry is empty -> no live pipeline. Any DB row still in an
		// in-progress status is orphaned from a previous crashed/killed
		// process; mark them failed so the UI doesn't keep showing
		// "trimming" forever and a new run can start immediately. Boot-time
		// MarkStaleGenerationsFailed only catches rows that exist BEFORE
		// the boot — this catches rows created AFTER boot whose goroutine
		// died.
		if n, derr := stateDB.MarkStaleGenerationsFailed(req.Context()); derr != nil {
			log.Printf("WARN: clean stale generations before new run: %v", derr)
		} else if n > 0 {
			log.Printf("cleaned %d stale generation row(s) before starting new run", n)
		}

		genID, err := stateDB.CreateGeneration(req.Context(), id)
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}

		// Run in background. Use a fresh context detached from the HTTP request
		// so closing the connection doesn't kill the pipeline mid-render. Cap
		// at 30 min. Begin claims the registry slot (cancellable from /cancel).
		runCtx, runCancel := context.WithTimeout(context.Background(), 30*time.Minute)
		regCtx, err := runRegistry.Begin(runCtx, genID)
		if err != nil {
			// Lost a race between IsBusy and Begin — mark the just-created row
			// failed so it doesn't sit forever in "queued".
			runCancel()
			failed := state.GenStatusFailed
			msg := err.Error()
			_ = stateDB.UpdateGeneration(req.Context(), genID, state.GenerationPatch{
				Status: &failed,
				Error:  &msg,
				Finish: true,
			})
			writeStudioJSON(w, http.StatusConflict, map[string]string{
				"code":    "ANOTHER_RUNNING",
				"message": "Another generation is already running.",
			})
			return
		}

		go func(projectID, generationID int64) {
			defer runCancel()
			defer runRegistry.End(generationID)
			// Panic safety net: if anything inside the pipeline panics
			// (driver crash, ffmpeg pipe weirdness, nil deref in a new
			// codepath), don't let the goroutine die silently. Mark the
			// row failed so the UI stops spinning and the operator can
			// retry. Also persist the panic message so /log captures it.
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("PANIC in pipeline goroutine (project=%d gen=%d): %v", projectID, generationID, rec)
					failed := state.GenStatusFailed
					msg := fmt.Sprintf("studio panic: %v — see studio.log for stack", rec)
					_ = stateDB.UpdateGeneration(context.Background(), generationID, state.GenerationPatch{
						Status: &failed,
						Error:  &msg,
						Finish: true,
					})
				}
			}()

			outputPath, err := pipelineRunner.Run(regCtx, projectID, generationID)
			if err != nil {
				log.Printf("pipeline run failed (project=%d gen=%d): %v", projectID, generationID, err)
				return
			}
			log.Printf("pipeline run done (project=%d gen=%d)", projectID, generationID)

			// Phase 7.1 — upload to cloud so the watch page has something to play.
			// Failures here are NOT pipeline failures — local render is fine, we
			// just couldn't ship it. Log + carry on; operator can retry by
			// re-running the generate.
			uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 35*time.Minute)
			defer uploadCancel()
			p, perr := stateDB.GetProject(uploadCtx, projectID)
			if perr != nil {
				log.Printf("WARN: post-render upload skipped (project lookup project=%d): %v", projectID, perr)
				return
			}
			if p.RemoteJumpID <= 0 {
				log.Printf("WARN: post-render upload skipped (project=%d has no remote_jump_id)", projectID)
				return
			}
			artID, jumpStatus, uerr := deliveryClient.UploadAndRegister(
				uploadCtx, p.RemoteJumpID, "horizontal_1080p", outputPath, 1920, 1080,
			)
			if uerr != nil {
				log.Printf("WARN: post-render upload failed (project=%d jump=%d): %v", projectID, p.RemoteJumpID, uerr)
				return
			}
			log.Printf("post-render upload OK (project=%d jump=%d artifact=%d jump_status=%s)",
				projectID, p.RemoteJumpID, artID, jumpStatus)
		}(id, genID)

		writeStudioJSON(w, http.StatusAccepted, map[string]any{
			"generation_id": genID,
			"status":        state.GenStatusQueued,
		})
	})

	// POST /generations/{id}/cancel — stop a running pipeline. ffmpeg dies
	// when the registered context is cancelled; the run goroutine then writes
	// status='failed' with error="cancelled by user".
	r.Post("/generations/{id}/cancel", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		if !runRegistry.Cancel(id) {
			writeStudioJSON(w, http.StatusNotFound, map[string]string{
				"code":    "NOT_RUNNING",
				"message": "That generation isn't currently running.",
			})
			return
		}
		// Best-effort row update — runner goroutine may also write the same
		// fields when ffmpeg exits, but doing it here makes the UI flip to
		// "failed" instantly without waiting for ffmpeg's death-rattle.
		failed := state.GenStatusFailed
		msg := "cancelled by user"
		_ = stateDB.UpdateGeneration(req.Context(), id, state.GenerationPatch{
			Status: &failed,
			Error:  &msg,
			Finish: true,
		})
		writeStudioJSON(w, http.StatusOK, map[string]string{"status": "cancelling"})
	})

	// GET /projects/{id}/generations — latest run status (UI polling target).
	r.Get("/projects/{id}/generations", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			writeStudioJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
			return
		}
		g, err := stateDB.GetLatestGeneration(req.Context(), id)
		if errors.Is(err, state.ErrNotFound) {
			writeStudioJSON(w, http.StatusOK, map[string]any{"generation": nil})
			return
		}
		if err != nil {
			writeStudioJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
			return
		}
		writeStudioJSON(w, http.StatusOK, map[string]any{
			"generation": map[string]any{
				"id":           g.ID,
				"status":       g.Status,
				"progress_pct": g.ProgressPct,
				"step_label":   g.StepLabel,
				"output_size":  g.OutputSize,
				"error":        g.Error,
				"started_at":   g.StartedAt,
				"finished_at":  g.FinishedAt,
			},
		})
	})

	// GET /projects/{id}/output/1080p — stream the produced 1080p MP4.
	// Range-aware (http.ServeFile handles it) so browser can scrub.
	r.Get("/projects/{id}/output/{kind}", func(w http.ResponseWriter, req *http.Request) {
		id, err := parseInt64URLParam(req, "id")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		kind := chi.URLParam(req, "kind")
		// MVP: only 1080p is produced. Future: 4k, vertical, photos.zip.
		if kind != "1080p" {
			http.Error(w, "unknown output kind", http.StatusNotFound)
			return
		}
		g, err := stateDB.GetLatestGeneration(req.Context(), id)
		if errors.Is(err, state.ErrNotFound) || g == nil || g.OutputPath == "" {
			http.NotFound(w, req)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !strings.HasPrefix(g.OutputPath, jobsDir+string(os.PathSeparator)) {
			http.Error(w, "output path is outside jobs directory", http.StatusForbidden)
			return
		}
		http.ServeFile(w, req, g.OutputPath)
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
// portFromAddr extracts the port from an HTTP listen addr string like ":8080"
// or "127.0.0.1:8080". The studio sidebar shows it as part of the "Studio ·
// localhost:8080" caption — purely cosmetic, falls back to the raw addr.
func portFromAddr(addr string) string {
	if addr == "" {
		return ""
	}
	i := -1
	for k := len(addr) - 1; k >= 0; k-- {
		if addr[k] == ':' {
			i = k
			break
		}
	}
	if i < 0 {
		return addr
	}
	return addr[i+1:]
}

// maskToken replaces the middle of a license token with bullets, leaving the
// 4-char prefix + suffix visible for identification.
func maskToken(t string) string {
	if t == "" {
		return "(not configured)"
	}
	if len(t) <= 12 {
		return "••••••••"
	}
	return t[:4] + "••••••••" + t[len(t)-4:]
}

type homeData struct {
	Version          string
	Platform         string
	Addr             string
	Port             string
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

// tailLog reads the last n lines of a log file. Reads at most 1 MB from
// the end, splits on \n, and returns the trailing nLines. On error returns
// the error message in the body so /log is always informative.
func tailLog(path string, nLines int) string {
	const maxRead = 1 << 20 // 1 MB tail buffer is plenty for a session
	f, err := os.Open(path)
	if err != nil {
		return "(open log: " + err.Error() + ")"
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "(stat log: " + err.Error() + ")"
	}
	size := st.Size()
	readFrom := int64(0)
	if size > maxRead {
		readFrom = size - maxRead
	}
	if _, err := f.Seek(readFrom, 0); err != nil {
		return "(seek: " + err.Error() + ")"
	}
	buf := make([]byte, size-readFrom)
	if _, err := io.ReadFull(f, buf); err != nil && !errors.Is(err, io.EOF) {
		return "(read: " + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if readFrom > 0 && len(lines) > 0 {
		// First line might be partial (we seeked into the middle of one).
		lines = lines[1:]
	}
	if len(lines) > nLines {
		lines = lines[len(lines)-nLines:]
	}
	return strings.Join(lines, "\n")
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

// brandingProvider adapts the studio branding cache to the
// pipeline.BrandingProviderLike interface. The pipeline doesn't know our
// tenant ID — it gets baked in here from the license manager's snapshot at
// render time. Returns an empty bundle (= no overlay) if the license isn't
// currently valid, so a stale-license render still produces an unbranded
// MP4 rather than failing outright.
type brandingProvider struct {
	cache   *studiobranding.Cache
	license *license.Manager
}

func (p *brandingProvider) EnsureForRun(ctx context.Context) (studiobranding.Bundle, error) {
	snap, _ := p.license.Snapshot()
	if !snap.Valid || snap.TenantID <= 0 {
		return studiobranding.Bundle{}, nil
	}
	return p.cache.Ensure(ctx, snap.TenantID)
}
