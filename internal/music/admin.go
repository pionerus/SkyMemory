// Package music owns the cloud-side music library — admin upload/list/delete
// today, studio-facing catalog/suggest endpoints in a follow-up.
//
// Tracks live in S3 (or MinIO in dev) under the bucket configured in
// FREEFALL_MUSIC_*. Database row points at the s3_key.
package music

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/pionerus/freefall/internal/auth"
	"github.com/pionerus/freefall/internal/db"
	"github.com/pionerus/freefall/internal/storage"
)

// Handlers wires the admin music endpoints. One instance constructed at boot.
type Handlers struct {
	DB      *db.Pool
	Storage *storage.Client
}

// =====================================================================
// POST /admin/music — upload a track
// Multipart form: field "file" (mp3), "title" (string), "artist" (optional),
// "license" (string), "mood" (csv: "epic,fun"), "suggested_for" (csv: "intro,main").
// Owner-only. Track lives globally — visible to ALL tenants.
// =====================================================================
func (h *Handlers) Upload(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	// Cap body at 100 MB — typical MP3 with a few minutes is 5-15 MB.
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_FORM", err.Error())
		return
	}

	f, fh, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "FILE_MISSING", "Form field 'file' is missing.")
		return
	}
	defer f.Close()

	const maxBytes = int64(100) << 20
	if fh.Size > maxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "Track exceeds 100 MB limit.")
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename))
	}
	if title == "" || len(title) > 200 {
		writeErr(w, http.StatusBadRequest, "INVALID_TITLE", "Title is required (≤200 chars).")
		return
	}

	artist := strings.TrimSpace(r.FormValue("artist"))
	license := strings.TrimSpace(r.FormValue("license"))
	if license == "" {
		license = "unknown"
	}
	mood := splitCSV(r.FormValue("mood"))
	suggestedFor := splitCSV(r.FormValue("suggested_for"))

	// Minimum-viable duration parsing — operator types it for now. We'll plug
	// ffprobe metadata extraction here in a follow-up so admin doesn't have to.
	durationSeconds, _ := strconv.Atoi(r.FormValue("duration_seconds"))
	if durationSeconds <= 0 {
		durationSeconds = 0
	}

	// Generate a salted S3 key to avoid leaking title/operator info via URL.
	keySalt := make([]byte, 8)
	if _, err := rand.Read(keySalt); err != nil {
		writeErr(w, http.StatusInternalServerError, "RAND_ERROR", err.Error())
		return
	}
	ext := filepath.Ext(fh.Filename)
	if ext == "" {
		ext = ".mp3"
	}
	s3Key := "global/music/" + hex.EncodeToString(keySalt) + ext

	contentType := fh.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "audio/mpeg"
	}

	uploadCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := h.Storage.PutObject(uploadCtx, s3Key, contentType, f, fh.Size); err != nil {
		writeErr(w, http.StatusInternalServerError, "S3_UPLOAD_FAILED", err.Error())
		return
	}

	dbCtx, cancel2 := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel2()

	var id int64
	err = h.DB.QueryRow(dbCtx, `
		INSERT INTO music_tracks
			(tenant_id, title, artist, license, s3_key, duration_seconds,
			 mood, suggested_for, uploaded_by)
		VALUES (NULL, $1, NULLIF($2,''), $3, $4, $5, $6, $7, $8)
		RETURNING id
	`,
		title, artist, license, s3Key, durationSeconds,
		mood, suggestedFor, s.OperatorID,
	).Scan(&id)
	if err != nil {
		// best-effort cleanup — orphan S3 object isn't dangerous, but try
		_ = h.Storage.DeleteObject(uploadCtx, s3Key)
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":               id,
		"title":            title,
		"artist":           artist,
		"license":          license,
		"duration_seconds": durationSeconds,
		"mood":             mood,
		"suggested_for":    suggestedFor,
		"s3_key":           s3Key,
	})
}

// =====================================================================
// GET /admin/music — list all tracks (active by default; ?include_inactive=1
// to see soft-deleted). Each row carries a 30-min presigned preview URL so
// admin can inline-play in <audio>.
// =====================================================================
type listedTrack struct {
	ID              int64    `json:"id"`
	Title           string   `json:"title"`
	Artist          string   `json:"artist,omitempty"`
	License         string   `json:"license"`
	DurationSeconds int      `json:"duration_seconds,omitempty"`
	Mood            []string `json:"mood,omitempty"`
	SuggestedFor    []string `json:"suggested_for,omitempty"`
	IsActive        bool     `json:"is_active"`
	CreatedAt       time.Time `json:"created_at"`
	S3Key           string   `json:"s3_key"`
	PreviewURL      string   `json:"preview_url,omitempty"`
}

func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	includeInactive := r.URL.Query().Get("include_inactive") == "1"

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	q := `SELECT id, title, COALESCE(artist,''), license, duration_seconds,
	             mood, suggested_for, is_active, created_at, s3_key
	      FROM music_tracks WHERE tenant_id IS NULL`
	if !includeInactive {
		q += ` AND is_active = true`
	}
	q += ` ORDER BY created_at DESC`

	rows, err := h.DB.Query(ctx, q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	defer rows.Close()

	out := []listedTrack{}
	for rows.Next() {
		var t listedTrack
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.License, &t.DurationSeconds,
			&t.Mood, &t.SuggestedFor, &t.IsActive, &t.CreatedAt, &t.S3Key); err != nil {
			writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}

		// Best-effort presign — if the bucket is wedged, leave URL empty and the
		// admin row still renders (just without inline preview).
		if url, perr := h.Storage.PresignGet(ctx, t.S3Key, 30*time.Minute); perr == nil {
			t.PreviewURL = url
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": out})
}

// =====================================================================
// DELETE /admin/music/{id}
// Soft-delete (is_active=false). Hard-delete via ?hard=1 also removes the
// S3 object. Soft is the default — preserves history if a track is referenced
// by old jumps.music_track_id.
// =====================================================================
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}
	hard := r.URL.Query().Get("hard") == "1"

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if hard {
		var s3Key string
		err := h.DB.QueryRow(ctx, `SELECT s3_key FROM music_tracks WHERE id = $1`, id).Scan(&s3Key)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", "Track not found.")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		// Drop S3 first; even if it fails, hard delete still nukes the row.
		_ = h.Storage.DeleteObject(ctx, s3Key)
		if _, err := h.DB.Exec(ctx, `DELETE FROM music_tracks WHERE id = $1`, id); err != nil {
			writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "hard_deleted"})
		return
	}

	tag, err := h.DB.Exec(ctx,
		`UPDATE music_tracks SET is_active = false WHERE id = $1 AND is_active = true`, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "Track not found or already inactive.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "soft_deleted"})
}

// =====================================================================
// helpers
// =====================================================================
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}

// splitCSV trims, lowers and de-empties a comma-separated list.
func splitCSV(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// (unused) keep error import alive when we later wire validation helpers
var _ = fmt.Sprintf
