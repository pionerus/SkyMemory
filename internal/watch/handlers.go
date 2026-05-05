// Package watch serves the public client-facing /watch/<access_code> page.
// No auth — the access_code itself is the bearer of authority. Phase 7.4.
//
// The page resolves the access_code → most-recent jump → most-recent
// horizontal_1080p artifact, presigns a 24h GET URL, and renders the
// `watch.html` template. Every page hit logs a `watch_events` row for the
// platform/club analytics dashboards.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/pionerus/freefall/internal/db"
	"github.com/pionerus/freefall/internal/storage"
)

// Handlers wires the public watch endpoints. Constructed once at boot;
// Render is the only externally-exposed handler.
type Handlers struct {
	DB             *db.Pool
	DeliverStorage *storage.Client // bucket holding rendered videos
	Templates      Renderer
}

// Renderer is the tiny interface to the cloud's HTML template registry.
// Defined here (rather than imported from web/server/templates) so this
// package stays a leaf. Matches html/template.Template.ExecuteTemplate.
type Renderer interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}

// PageData is what watch.html receives.
type PageData struct {
	ClientName     string
	JumpDate       time.Time
	Status         string
	HasVideo       bool
	VideoURL       string // main edit (1080p OR 4K) — 24h presigned GET, empty if no artifact yet
	VideoSizeBytes int64  // for the download card subtitle
	VideoLabel     string // "1080p" / "2K" / "4K" — what the operator actually rendered
	AccessCode     string // dashed format for display: "XXXX-XXXX"
	TenantName     string // dropzone name for the topbar
	OperatorName   string // "Filmed by …" — derived from operators.email prefix
	NotFound       bool   // true → "Sorry, that link isn't valid"

	// Phase 5 short-form deliverables. Empty URL = no artifact yet, template
	// keeps the "Coming soon" stub visible. Non-empty URL flips to a live
	// download card.
	VerticalReelURL  string
	VerticalReelSize int64
	WOWReelURL       string
	WOWReelSize      int64

	// Phase 5 photo pack. Empty slice → photo grid section is hidden.
	// Capped to 20 in handler so a misbehaving studio upload can't blow
	// up rendering.
	Photos []Photo
}

// Photo is one freefall still surfaced in the wt-photo-grid section.
type Photo struct {
	URL       string
	SizeBytes int64
	Width     int
	Height    int
}

// Render handles GET /watch/{access_code}. The access_code in the URL is
// the canonical 8-char form (no dash).
func (h *Handlers) Render(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "access_code")
	canon := canonicalAccessCode(raw)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // presigned URL is short-lived

	if !validateAccessCode(canon) {
		_ = h.Templates.ExecuteTemplate(w, "watch.html", PageData{NotFound: true})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Resolve access_code → latest jump for that client. One client may have
	// multiple jumps; the public watch link shows the most recent.
	// Joins tenants + operators so the page can show "Aero Club Ural" /
	// "Filmed by <operator>" without a second round-trip.
	var (
		jumpID        int64
		clientName    string
		clientID      int64
		createdAt     time.Time
		status        string
		tenantName    string
		operatorEmail string
	)
	err := h.DB.QueryRow(ctx, `
		SELECT j.id, c.name, c.id, j.created_at, j.status,
		       COALESCE(t.name, ''), COALESCE(o.email, '')
		FROM clients c
		JOIN jumps j ON j.client_id = c.id
		JOIN tenants t ON t.id = j.tenant_id
		LEFT JOIN operators o ON o.id = j.operator_id
		WHERE c.access_code = $1
		ORDER BY j.created_at DESC
		LIMIT 1`,
		canon,
	).Scan(&jumpID, &clientName, &clientID, &createdAt, &status, &tenantName, &operatorEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = h.Templates.ExecuteTemplate(w, "watch.html", PageData{NotFound: true})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := PageData{
		ClientName:   clientName,
		JumpDate:     createdAt,
		Status:       status,
		AccessCode:   dashAccessCode(canon),
		TenantName:   tenantName,
		OperatorName: operatorNameFor(operatorEmail),
	}

	// Pull every artifact for this jump in one query — main video + reels +
	// photos. ORDER BY puts the canonical pick first per kind (variant
	// 'original' > 'preview', then most-recently-uploaded). We then walk
	// the rows in Go: keep the first match per non-photo kind, accumulate
	// up to 20 photos. Each presign error is best-effort — log + skip,
	// never fail the whole page.
	const watchTTL = 24 * time.Hour
	const maxPhotos = 20
	rows, err := h.DB.Query(ctx, `
		SELECT kind, s3_key, size_bytes, COALESCE(width, 0), COALESCE(height, 0)
		FROM jump_artifacts
		WHERE jump_id = $1
		  AND kind IN ('horizontal_1080p','horizontal_4k','vertical','wow_highlights','photo')
		ORDER BY kind,
		         (variant = 'original') DESC,
		         uploaded_at DESC`,
		jumpID,
	)
	if err == nil {
		defer rows.Close()
		seenMain, seenVert, seenWOW := false, false, false
		for rows.Next() {
			var (
				kind          string
				s3Key         string
				sizeBytes     int64
				width, height int
			)
			if scanErr := rows.Scan(&kind, &s3Key, &sizeBytes, &width, &height); scanErr != nil {
				continue
			}
			if s3Key == "" {
				continue
			}
			// Drive-hosted artifacts carry a "drive:<fileId>" key.
			// The file was made public on upload so we return a direct URL.
			resolveURL := func(key string) (string, bool) {
				if strings.HasPrefix(key, "drive:") {
					return "https://drive.google.com/uc?export=download&id=" + key[6:], true
				}
				u, err := h.DeliverStorage.PresignGet(ctx, key, watchTTL)
				return u, err == nil
			}

			switch kind {
			case "horizontal_4k", "horizontal_1080p":
				// Main video. We treat both kinds as the same slot — operator
				// renders ONE main video at chosen resolution. Iteration order
				// is alphabetical by kind, so horizontal_4k naturally wins
				// over horizontal_1080p when both somehow exist (re-render
				// edge case). seenMain flag short-circuits the second.
				if seenMain {
					continue
				}
				seenMain = true
				if url, ok := resolveURL(s3Key); ok {
					data.HasVideo = true
					data.VideoURL = url
					data.VideoSizeBytes = sizeBytes
					data.VideoLabel = videoLabelFor(height)
				}
			case "vertical":
				if seenVert {
					continue
				}
				seenVert = true
				if url, ok := resolveURL(s3Key); ok {
					data.VerticalReelURL = url
					data.VerticalReelSize = sizeBytes
				}
			case "wow_highlights":
				if seenWOW {
					continue
				}
				seenWOW = true
				if url, ok := resolveURL(s3Key); ok {
					data.WOWReelURL = url
					data.WOWReelSize = sizeBytes
				}
			case "photo":
				if len(data.Photos) >= maxPhotos {
					continue
				}
				if url, ok := resolveURL(s3Key); ok {
					data.Photos = append(data.Photos, Photo{
						URL:       url,
						SizeBytes: sizeBytes,
						Width:     width,
						Height:    height,
					})
				}
			}
		}
	}

	// watch_events insert — fire-and-forget so a slow log row doesn't
	// delay the page. Dedupe-within-session is the platform-admin
	// concern (Phase 10.5); we just record raw hits here.
	go func(jumpID int64, status string, ua, referer string, ip string) {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel2()
		sessionHash := sessionHashFor(ua, ip)
		// Insert only when we'll actually serve content — failed lookup
		// already 404'd. artifact_kind reflects what we tried to play.
		_, _ = h.DB.Exec(ctx2, `
			INSERT INTO watch_events (jump_id, artifact_kind, referrer, user_agent, ip, session_hash)
			VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5::inet, $6)`,
			jumpID, "horizontal_1080p", referer, ua, ip, sessionHash,
		)
	}(jumpID, status, r.UserAgent(), r.Referer(), clientIPFor(r))

	_ = h.Templates.ExecuteTemplate(w, "watch.html", data)
}

// TrackDownload handles POST /watch/{access_code}/download — fired by
// watch.html JS when the visitor clicks any "Download" button. Idempotently
// stamps jumps.download_clicked_at on the first hit; subsequent clicks
// no-op. Drives the canonical "downloaded" status in v_client_status.
//
// Access_code in the URL IS the bearer — same trust model as Render.
// Returns 204 on success, 404 if the code maps to nothing.
func (h *Handlers) TrackDownload(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "access_code")
	canon := canonicalAccessCode(raw)
	if !validateAccessCode(canon) {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// Find the most-recent jump for this access_code. Use the same
	// scoping as Render so an old/rotated code can't be retroactively
	// "downloaded".
	var jumpID int64
	err := h.DB.QueryRow(ctx, `
		SELECT j.id
		FROM clients c
		JOIN jumps   j ON j.client_id = c.id
		WHERE c.access_code = $1
		ORDER BY j.created_at DESC
		LIMIT 1`,
		canon,
	).Scan(&jumpID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// First-click-wins: COALESCE preserves the original timestamp on
	// repeat downloads so the lifecycle reads as "first opened at X".
	_, _ = h.DB.Exec(ctx, `
		UPDATE jumps
		   SET download_clicked_at = COALESCE(download_clicked_at, NOW())
		 WHERE id = $1`,
		jumpID,
	)

	// Also drop a watch_event so platform analytics can break it down.
	go func(jumpID int64, ua, ref, ip string) {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel2()
		_, _ = h.DB.Exec(ctx2, `
			INSERT INTO watch_events (jump_id, artifact_kind, referrer, user_agent, ip, session_hash)
			VALUES ($1, 'download', NULLIF($2,''), NULLIF($3,''), $4::inet, $5)`,
			jumpID, ref, ua, ip, sessionHashFor(ua, ip),
		)
	}(jumpID, r.UserAgent(), r.Referer(), clientIPFor(r))

	w.WriteHeader(http.StatusNoContent)
}

// videoLabelFor maps a video's height to the marketing label shown next
// to the Full edit download on the watch page.
func videoLabelFor(h int) string {
	switch {
	case h >= 2160:
		return "4K"
	case h >= 1440:
		return "2K"
	default:
		return "1080p"
	}
}

// operatorNameFor turns "andrey@aeroclub.ru" into "Andrey". The operators
// table doesn't carry a display name yet (Phase 11+ will add it), so we
// derive a friendly form from the local-part of the email. Falls back to
// "the camera operator" when the email is empty or only special characters.
func operatorNameFor(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "the camera operator"
	}
	local := email[:at]
	// Strip a leading dot (rare) and replace separators with space.
	local = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(local)
	local = strings.TrimSpace(local)
	if local == "" {
		return "the camera operator"
	}
	// Title-case the first word; "andrey vasiliev" → "Andrey Vasiliev".
	parts := strings.Fields(local)
	for i, p := range parts {
		if len(p) == 0 {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// canonicalAccessCode strips dashes and uppercases. The DB stores them
// canonical; the URL might have them either way.
func canonicalAccessCode(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	return strings.ToUpper(s)
}

// dashAccessCode formats "ABCD1234" as "ABCD-1234" for the page header.
func dashAccessCode(canon string) string {
	if len(canon) == 8 {
		return canon[:4] + "-" + canon[4:]
	}
	return canon
}

// validateAccessCode is a defense-in-depth check before any DB lookup. The
// alphabet is Crockford Base32 minus I/L/O/U; we just check length + that
// every char is alnum upper.
func validateAccessCode(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		ok := (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

// sessionHashFor produces a coarse fingerprint so platform-admin analytics
// can dedupe a single visitor's repeated F5 hits without storing PII.
func sessionHashFor(userAgent, ip string) string {
	h := sha256.Sum256([]byte(userAgent + "|" + ip))
	return hex.EncodeToString(h[:8])
}

// clientIPFor extracts the request IP, preferring X-Forwarded-For when
// present (Caddy/Cloudflare-style reverse proxies). Returns "0.0.0.0" if
// nothing parseable so the watch_events.ip cast doesn't fail.
func clientIPFor(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// First entry = original client.
		if i := strings.IndexByte(xff, ','); i > 0 {
			xff = xff[:i]
		}
		if ip := net.ParseIP(strings.TrimSpace(xff)); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return "0.0.0.0"
}
