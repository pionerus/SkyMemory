package music

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
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
// POST /api/v1/music/suggest — top-N picks for a project.
//
// Scoring inputs (req body):
//   duration_seconds — target final-video duration. We prefer tracks at least
//                      as long (we'll trim the tail in the renderer); we
//                      penalise tracks that are too short or absurdly long.
//   mood             — optional array; each match adds points.
//   limit            — defaults to 3, cap 10.
//
// Scoring (all summed):
//   +10 per overlapping mood tag
//   +5  if track has 'main' in suggested_for (vs. only intro/outro)
//   +5  if track.duration ≥ target (we don't have to loop)
//   +0..5 closeness penalty: 5 * (1 - |track.dur - target| / max(track.dur, target))
//
// Ties broken by random() so the operator gets variety. Reuses StudioCatalog's
// (NULL OR tenant_id = $1) visibility scope so a tenant never sees another
// tenant's private uploads.
// =====================================================================
func (h *Handlers) StudioSuggest(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	var req v1.MusicSuggestRequest
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req) // best-effort; missing/malformed → default
	}
	if req.Limit <= 0 || req.Limit > 10 {
		req.Limit = 3
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Fetch the visible catalog. Score in Go (clearer than a 30-line CASE
	// expression and we have at most a few hundred tracks per tenant).
	rows, err := h.DB.Query(ctx, `
		SELECT id, title, COALESCE(artist,''), license, duration_seconds, COALESCE(bpm,0),
		       mood, suggested_for, s3_key
		FROM music_visible_to
		WHERE tenant_id IS NULL OR tenant_id = $1`,
		s.TenantID,
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	defer rows.Close()

	type candidate struct {
		track v1.MusicTrack
		s3Key string
	}
	var all []candidate
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
		all = append(all, candidate{track: t, s3Key: s3Key})
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if len(all) == 0 {
		writeJSON(w, http.StatusOK, v1.MusicSuggestResponse{Tracks: []v1.MusicTrack{}})
		return
	}

	// Score each candidate.
	for i := range all {
		c := &all[i]
		c.track.Score, c.track.Reason = scoreTrack(c.track, req.DurationSeconds, req.Mood)
	}

	// Stable sort: score DESC, then random tiebreak. We add a tiny random nudge
	// to scores so ties shuffle within the same suggest call. Cheap variety.
	sort.SliceStable(all, func(i, j int) bool {
		si := all[i].track.Score + jitter(all[i].track.ID)
		sj := all[j].track.Score + jitter(all[j].track.ID)
		return si > sj
	})

	if req.Limit > len(all) {
		req.Limit = len(all)
	}
	out := make([]v1.MusicTrack, 0, req.Limit)
	for i := 0; i < req.Limit; i++ {
		t := all[i].track
		if url, perr := h.Storage.PresignGet(ctx, all[i].s3Key, 15*time.Minute); perr == nil {
			t.PreviewURL = url
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, v1.MusicSuggestResponse{Tracks: out})
}

// scoreTrack returns the heuristic score and a human reason string.
//
// `target` may be 0 — that means the operator hasn't uploaded any clips yet
// and we have nothing to compare track length against. In that case we still
// score on mood + 'main' tag.
func scoreTrack(t v1.MusicTrack, target int, mood []string) (float64, string) {
	score := 0.0
	reasons := []string{}

	// Mood overlap.
	moodMatches := 0
	if len(mood) > 0 {
		want := map[string]bool{}
		for _, m := range mood {
			want[strings.ToLower(strings.TrimSpace(m))] = true
		}
		for _, m := range t.Mood {
			if want[strings.ToLower(m)] {
				moodMatches++
				score += 10
			}
		}
		if moodMatches > 0 {
			reasons = append(reasons, fmt.Sprintf("matched mood %s", joinStrings(intersectMood(mood, t.Mood), ", ")))
		}
	}

	// 'main' bias — tracks tagged for the body of the video over intro/outro fillers.
	for _, sf := range t.SuggestedFor {
		if strings.EqualFold(sf, "main") {
			score += 5
			reasons = append(reasons, "tagged for main score")
			break
		}
	}

	// Duration vs target.
	if target > 0 && t.DurationSeconds > 0 {
		td := float64(t.DurationSeconds)
		tg := float64(target)

		if td >= tg {
			score += 5
			reasons = append(reasons, fmt.Sprintf("track %ds covers project %ds", t.DurationSeconds, target))
		} else {
			reasons = append(reasons, fmt.Sprintf("track %ds shorter than project %ds", t.DurationSeconds, target))
		}
		// Closeness 0..5: identical → 5, double-or-half → ~2.5, 10x off → ~0.5.
		denom := math.Max(td, tg)
		if denom > 0 {
			score += 5.0 * (1 - math.Abs(td-tg)/denom)
		}
	}

	if len(reasons) == 0 {
		reasons = []string{"baseline pick"}
	}
	return score, joinStrings(reasons, "; ")
}

// intersectMood returns elements present in both lists, case-insensitive.
func intersectMood(a, b []string) []string {
	have := map[string]bool{}
	for _, x := range a {
		have[strings.ToLower(x)] = true
	}
	out := []string{}
	for _, y := range b {
		if have[strings.ToLower(y)] {
			out = append(out, y)
		}
	}
	return out
}

// jitter returns a small reproducible value (±0.5) seeded by track id —
// breaks ties without flapping every render.
func jitter(id int64) float64 {
	return float64((id*2654435761)%1000) / 2000.0 // 0..0.5
}

func joinStrings(xs []string, sep string) string {
	return strings.Join(xs, sep)
}

