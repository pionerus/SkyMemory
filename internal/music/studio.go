package music

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	v1 "github.com/pionerus/freefall/internal/api/v1"
	"github.com/pionerus/freefall/internal/auth"
)

// =====================================================================
// GET /api/v1/music — studio-facing catalog.
// Auth: license token (RequireLicenseToken sets the SessionData in ctx).
// Returns active tracks visible to the calling tenant — that's NULL (global)
// plus tenant-owned. Each row carries a 15-minute presigned preview URL.
// =====================================================================
func (h *Handlers) StudioCatalog(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.DB.Query(ctx, `
		SELECT id, title, COALESCE(artist,''), license, duration_seconds, COALESCE(bpm,0),
		       mood, suggested_for, s3_key
		FROM music_visible_to
		WHERE tenant_id IS NULL OR tenant_id = $1
		ORDER BY created_at DESC`,
		s.TenantID,
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	defer rows.Close()

	out := []v1.MusicTrack{}
	for rows.Next() {
		var (
			t     v1.MusicTrack
			s3Key string
		)
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.License,
			&t.DurationSeconds, &t.BPM, &t.Mood, &t.SuggestedFor, &s3Key); err != nil {
			writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		// Best-effort presigning — empty preview_url means studio shows the row
		// without an inline player; the actual fetch URL is generated lazily on pick.
		if url, perr := h.Storage.PresignGet(ctx, s3Key, 15*time.Minute); perr == nil {
			t.PreviewURL = url
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, v1.MusicListResponse{Tracks: out})
}

// =====================================================================
// POST /api/v1/music/suggest — top-3 (or N) picks for a project.
// MVP: random sample from the catalog. Future: filter by duration window,
// mood overlap, BPM range. Reusing StudioCatalog's visibility scope.
// =====================================================================
func (h *Handlers) StudioSuggest(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	// We accept the body (duration, mood, limit) but ignore filters in MVP.
	// Limit defaults to 3.
	var req v1.MusicSuggestRequest
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req) // best-effort; missing/malformed → default
	}
	if req.Limit <= 0 || req.Limit > 10 {
		req.Limit = 3
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.DB.Query(ctx, `
		SELECT id, title, COALESCE(artist,''), license, duration_seconds, COALESCE(bpm,0),
		       mood, suggested_for, s3_key
		FROM music_visible_to
		WHERE tenant_id IS NULL OR tenant_id = $1
		ORDER BY random()
		LIMIT $2`,
		s.TenantID, req.Limit,
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	defer rows.Close()

	out := []v1.MusicTrack{}
	for rows.Next() {
		var (
			t     v1.MusicTrack
			s3Key string
		)
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.License,
			&t.DurationSeconds, &t.BPM, &t.Mood, &t.SuggestedFor, &s3Key); err != nil {
			writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		if url, perr := h.Storage.PresignGet(ctx, s3Key, 15*time.Minute); perr == nil {
			t.PreviewURL = url
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, v1.MusicSuggestResponse{Tracks: out})
}

