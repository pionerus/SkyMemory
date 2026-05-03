// Package branding owns per-tenant visual identity overlays — watermark
// PNG, intro snippet, outro snippet — that the studio pipeline applies to
// every render. Endpoints live under /admin/branding/* and require role=owner.
//
// Storage layout (FREEFALL_BRANDING_BUCKET, default "freefall-branding"):
//
//	<tenant_id>/watermark.png
//	<tenant_id>/intro.mp4
//	<tenant_id>/outro.mp4
//
// Settings (size_pct, opacity_pct, position) live in the tenants table.
package branding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	v1 "github.com/pionerus/freefall/internal/api/v1"
	"github.com/pionerus/freefall/internal/auth"
	"github.com/pionerus/freefall/internal/db"
	"github.com/pionerus/freefall/internal/storage"
)

// Handlers wires the branding admin endpoints. Constructed once at startup.
type Handlers struct {
	DB      *db.Pool
	Storage *storage.Client
}

// Settings is the JSON-shaped view of one tenant's branding row.
type Settings struct {
	WatermarkLogoURL    string `json:"watermark_logo_url,omitempty"`    // presigned, 30-min TTL — null if none uploaded
	WatermarkSizePct    int    `json:"watermark_size_pct"`              // 5–25
	WatermarkOpacityPct int    `json:"watermark_opacity_pct"`           // 10–100
	WatermarkPosition   string `json:"watermark_position"`              // 'bottom-right' | 'bottom-left' | 'top-right' | 'top-left'
	IntroClipURL        string `json:"intro_clip_url,omitempty"`        // presigned, 30-min TTL
	OutroClipURL        string `json:"outro_clip_url,omitempty"`        // presigned, 30-min TTL
}

// =====================================================================
// GET /admin/branding
// =====================================================================
func (h *Handlers) Get(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	settings, err := h.loadSettings(ctx, s.TenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// =====================================================================
// PUT /admin/branding
// JSON body: { watermark_size_pct, watermark_opacity_pct, watermark_position }
// =====================================================================
func (h *Handlers) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	var req struct {
		SizePct    int    `json:"watermark_size_pct"`
		OpacityPct int    `json:"watermark_opacity_pct"`
		Position   string `json:"watermark_position"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if req.SizePct < 5 || req.SizePct > 25 {
		writeErr(w, http.StatusBadRequest, "INVALID_SIZE", "watermark_size_pct must be 5..25")
		return
	}
	if req.OpacityPct < 10 || req.OpacityPct > 100 {
		writeErr(w, http.StatusBadRequest, "INVALID_OPACITY", "watermark_opacity_pct must be 10..100")
		return
	}
	switch req.Position {
	case "bottom-right", "bottom-left", "top-right", "top-left":
	default:
		writeErr(w, http.StatusBadRequest, "INVALID_POSITION", "watermark_position must be one of bottom-right, bottom-left, top-right, top-left")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := h.DB.Exec(ctx, `
		UPDATE tenants
		SET watermark_size_pct = $1,
		    watermark_opacity_pct = $2,
		    watermark_position = $3
		WHERE id = $4`,
		req.SizePct, req.OpacityPct, req.Position, s.TenantID,
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	settings, err := h.loadSettings(ctx, s.TenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// =====================================================================
// POST /admin/branding/watermark
// Multipart form: field "file" (PNG with alpha, ≤ 2 MB).
// Replaces any prior watermark — old key gets overwritten in place.
// =====================================================================
func (h *Handlers) UploadWatermark(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	const maxBytes = int64(2) << 20 // 2 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	if err := r.ParseMultipartForm(maxBytes + (1 << 20)); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_FORM", err.Error())
		return
	}

	f, fh, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "FILE_MISSING", "Form field 'file' is missing.")
		return
	}
	defer f.Close()

	if fh.Size > maxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "Watermark exceeds 2 MB limit.")
		return
	}

	// Sniff the first 512 bytes to confirm it's a PNG. Other formats might
	// work in ffmpeg overlay but we want predictable transparency behaviour.
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if !isPNG(head) {
		writeErr(w, http.StatusUnsupportedMediaType, "PNG_REQUIRED", "Watermark must be a PNG with alpha channel.")
		return
	}
	// Rewind for the upload.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		writeErr(w, http.StatusInternalServerError, "SEEK_ERROR", err.Error())
		return
	}

	key := fmt.Sprintf("%d/watermark.png", s.TenantID)
	uploadCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := h.Storage.PutObject(uploadCtx, key, "image/png", f, fh.Size); err != nil {
		writeErr(w, http.StatusInternalServerError, "STORAGE_ERROR", err.Error())
		return
	}

	if _, err := h.DB.Exec(uploadCtx,
		`UPDATE tenants SET watermark_logo_path = $1 WHERE id = $2`,
		key, s.TenantID,
	); err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	settings, err := h.loadSettings(uploadCtx, s.TenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// =====================================================================
// DELETE /admin/branding/watermark
// =====================================================================
func (h *Handlers) DeleteWatermark(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var key string
	err := h.DB.QueryRow(ctx,
		`SELECT watermark_logo_path FROM tenants WHERE id = $1`, s.TenantID,
	).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) || key == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no-op"})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	// Best-effort delete from S3 — DB row updated regardless so the operator
	// can re-upload immediately.
	_ = h.Storage.DeleteObject(ctx, key)

	if _, err := h.DB.Exec(ctx,
		`UPDATE tenants SET watermark_logo_path = NULL WHERE id = $1`,
		s.TenantID,
	); err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// =====================================================================
// POST /admin/branding/intro and POST /admin/branding/outro
// Multipart form: field "file" (mp4 / mov, ≤ 20 MB, ≤ 5 seconds — operator's
// responsibility to keep it short for now; ffprobe-side validation lands in
// a later session).
// =====================================================================
func (h *Handlers) UploadIntro(w http.ResponseWriter, r *http.Request) { h.uploadClip(w, r, "intro") }
func (h *Handlers) UploadOutro(w http.ResponseWriter, r *http.Request) { h.uploadClip(w, r, "outro") }
func (h *Handlers) DeleteIntro(w http.ResponseWriter, r *http.Request) { h.deleteClip(w, r, "intro") }
func (h *Handlers) DeleteOutro(w http.ResponseWriter, r *http.Request) { h.deleteClip(w, r, "outro") }

func (h *Handlers) uploadClip(w http.ResponseWriter, r *http.Request, slot string) {
	s := auth.MustFromContext(r.Context())

	const maxBytes = int64(20) << 20 // 20 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	if err := r.ParseMultipartForm(maxBytes + (1 << 20)); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_FORM", err.Error())
		return
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "FILE_MISSING", "Form field 'file' is missing.")
		return
	}
	defer f.Close()
	if fh.Size > maxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", fmt.Sprintf("%s clip exceeds 20 MB limit.", slot))
		return
	}

	key := fmt.Sprintf("%d/%s.mp4", s.TenantID, slot)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := h.Storage.PutObject(ctx, key, "video/mp4", f, fh.Size); err != nil {
		writeErr(w, http.StatusInternalServerError, "STORAGE_ERROR", err.Error())
		return
	}

	col := slot + "_clip_path"
	if _, err := h.DB.Exec(ctx,
		fmt.Sprintf(`UPDATE tenants SET %s = $1 WHERE id = $2`, col),
		key, s.TenantID,
	); err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	settings, err := h.loadSettings(ctx, s.TenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (h *Handlers) deleteClip(w http.ResponseWriter, r *http.Request, slot string) {
	s := auth.MustFromContext(r.Context())
	col := slot + "_clip_path"

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var key string
	err := h.DB.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM tenants WHERE id = $1`, col),
		s.TenantID,
	).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) || key == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no-op"})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	_ = h.Storage.DeleteObject(ctx, key)

	if _, err := h.DB.Exec(ctx,
		fmt.Sprintf(`UPDATE tenants SET %s = NULL WHERE id = $1`, col),
		s.TenantID,
	); err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// =====================================================================
// GET /api/v1/tenant/branding   (license-token auth, used by studio.exe)
// =====================================================================
//
// Studio calls this once per render to discover what branding to apply.
// Response only includes slots the operator has uploaded; absent slots are
// omitted so studio can range over present ones without nil-checks.
//
// `etag` lets studio cache the binary blobs across runs and re-download only
// when the operator replaces the asset.
func (h *Handlers) GetForStudio(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var (
		watermarkPath string
		introPath     string
		outroPath     string
		sizePct       int
		opacityPct    int
		position      string
	)
	err := h.DB.QueryRow(ctx, `
		SELECT
			COALESCE(watermark_logo_path, ''),
			watermark_size_pct,
			watermark_opacity_pct,
			watermark_position,
			COALESCE(intro_clip_path, ''),
			COALESCE(outro_clip_path, '')
		FROM tenants WHERE id = $1`,
		s.TenantID,
	).Scan(&watermarkPath, &sizePct, &opacityPct, &position, &introPath, &outroPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	const presignTTL = 30 * time.Minute
	out := v1.TenantBrandingResponse{TenantID: s.TenantID}

	if watermarkPath != "" {
		etag, _ := h.Storage.HeadETag(ctx, watermarkPath)
		url, perr := h.Storage.PresignGet(ctx, watermarkPath, presignTTL)
		if perr == nil {
			out.Watermark = &v1.WatermarkAsset{
				URL:        url,
				ETag:       etag,
				SizePct:    sizePct,
				OpacityPct: opacityPct,
				Position:   position,
			}
		}
	}
	if introPath != "" {
		etag, _ := h.Storage.HeadETag(ctx, introPath)
		if url, perr := h.Storage.PresignGet(ctx, introPath, presignTTL); perr == nil {
			out.Intro = &v1.BrandingClipAsset{URL: url, ETag: etag}
		}
	}
	if outroPath != "" {
		etag, _ := h.Storage.HeadETag(ctx, outroPath)
		if url, perr := h.Storage.PresignGet(ctx, outroPath, presignTTL); perr == nil {
			out.Outro = &v1.BrandingClipAsset{URL: url, ETag: etag}
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// =====================================================================
// internals
// =====================================================================

func (h *Handlers) loadSettings(ctx context.Context, tenantID int64) (Settings, error) {
	var (
		s             Settings
		watermarkPath string
		introPath     string
		outroPath     string
	)
	err := h.DB.QueryRow(ctx, `
		SELECT
			COALESCE(watermark_logo_path,    ''),
			watermark_size_pct,
			watermark_opacity_pct,
			watermark_position,
			COALESCE(intro_clip_path, ''),
			COALESCE(outro_clip_path, '')
		FROM tenants WHERE id = $1`, tenantID,
	).Scan(&watermarkPath, &s.WatermarkSizePct, &s.WatermarkOpacityPct, &s.WatermarkPosition, &introPath, &outroPath)
	if err != nil {
		return s, err
	}

	const presignTTL = 30 * time.Minute
	if watermarkPath != "" {
		url, perr := h.Storage.PresignGet(ctx, watermarkPath, presignTTL)
		if perr == nil {
			s.WatermarkLogoURL = url
		}
	}
	if introPath != "" {
		if url, perr := h.Storage.PresignGet(ctx, introPath, presignTTL); perr == nil {
			s.IntroClipURL = url
		}
	}
	if outroPath != "" {
		if url, perr := h.Storage.PresignGet(ctx, outroPath, presignTTL); perr == nil {
			s.OutroClipURL = url
		}
	}
	return s, nil
}

func isPNG(b []byte) bool {
	return len(b) >= 8 &&
		b[0] == 0x89 && b[1] == 0x50 && b[2] == 0x4E && b[3] == 0x47 &&
		b[4] == 0x0D && b[5] == 0x0A && b[6] == 0x1A && b[7] == 0x0A
}

// JSON response helpers — match the rest of the cloud admin's shape.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}

// reserve `strings` import in case future validators need it (tenant-name
// sanity etc); silence the linter for now.
var _ = strings.TrimSpace
