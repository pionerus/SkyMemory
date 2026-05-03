package jump

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	v1 "github.com/pionerus/freefall/internal/api/v1"
	"github.com/pionerus/freefall/internal/auth"
	"github.com/pionerus/freefall/internal/db"
	"github.com/pionerus/freefall/internal/storage"
)

// allowedArtifactKinds mirrors the CHECK constraint on jump_artifacts.kind.
// We reject other values BEFORE going to S3 so a typo doesn't waste a PUT
// followed by an INSERT-fails rollback.
var allowedArtifactKinds = map[string]string{
	"horizontal_1080p": "video/mp4",
	"horizontal_4k":    "video/mp4",
	"vertical":         "video/mp4",
	"photo":            "image/jpeg",
	"screenshot":       "image/jpeg",
}

// ArtifactsHandlers groups the upload-flow endpoints. Wired separately from
// jump.Handlers so cmd/server/main.go can keep the deliverables S3 client
// scoped to artifact uploads (vs. music + branding clients held elsewhere).
type ArtifactsHandlers struct {
	DB      *db.Pool
	Storage *storage.Client
}

// =====================================================================
// POST /api/v1/jumps/{id}/artifacts/upload-url
//
// Presigns an S3 PUT URL the studio uploads directly to. Cloud stays out
// of the byte-pump path. Returns the URL + the s3 key the studio echoes
// back when registering the artifact.
// =====================================================================
func (h *ArtifactsHandlers) RequestUploadURL(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	jumpID, err := parseInt64Param(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}

	var req v1.ArtifactUploadURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	contentType, ok := allowedArtifactKinds[req.Kind]
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_KIND",
			"kind must be one of: horizontal_1080p, horizontal_4k, vertical, photo, screenshot")
		return
	}
	// Cap size at 4 GB. A finished 4K render lands well under 1 GB; this is
	// just a defense against runaway studio bugs filling the bucket.
	if req.SizeBytes <= 0 || req.SizeBytes > (4<<30) {
		writeError(w, http.StatusBadRequest, "INVALID_SIZE", "size_bytes must be 1..4 GiB")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Confirm the jump belongs to this tenant before signing anything.
	var exists bool
	err = h.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM jumps WHERE id = $1 AND tenant_id = $2)`,
		jumpID, s.TenantID,
	).Scan(&exists)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Jump not found in this tenant.")
		return
	}

	ext := extensionFor(req.Kind)
	s3Key := fmt.Sprintf("%d/jumps/%d/%s%s", s.TenantID, jumpID, req.Kind, ext)

	const ttl = 30 * time.Minute
	uploadURL, err := h.Storage.PresignPut(ctx, s3Key, contentType, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PRESIGN_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, v1.ArtifactUploadURLResponse{
		UploadURL:    uploadURL,
		S3Key:        s3Key,
		ContentType:  contentType,
		ExpiresInSec: int(ttl / time.Second),
	})
}

// =====================================================================
// POST /api/v1/jumps/{id}/artifacts
//
// Studio calls this after the S3 PUT succeeds. Inserts a jump_artifacts
// row and (if there wasn't one already) bumps jump.status='ready'.
// Idempotent enough — same kind+s3_key gets a fresh row each time, last
// write wins on the watch page.
// =====================================================================
func (h *ArtifactsHandlers) RegisterArtifact(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	jumpID, err := parseInt64Param(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}

	var req v1.ArtifactRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if _, ok := allowedArtifactKinds[req.Kind]; !ok {
		writeError(w, http.StatusBadRequest, "INVALID_KIND", "Unknown artifact kind.")
		return
	}
	if req.SizeBytes <= 0 || req.S3Key == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELDS", "size_bytes and s3_key are required.")
		return
	}
	// S3 ETags occasionally arrive wrapped in quotes; strip so we store one canonical form.
	req.ETag = strings.Trim(req.ETag, `"`)
	if req.Variant == "" {
		req.Variant = "original"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Tenant scope check (same shape as RequestUploadURL).
	var exists bool
	err = h.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM jumps WHERE id = $1 AND tenant_id = $2)`,
		jumpID, s.TenantID,
	).Scan(&exists)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Jump not found in this tenant.")
		return
	}

	var artifactID int64
	err = h.DB.QueryRow(ctx, `
		INSERT INTO jump_artifacts (jump_id, kind, variant, s3_key, etag, size_bytes, width, height)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7,0), NULLIF($8,0))
		RETURNING id`,
		jumpID, req.Kind, req.Variant, req.S3Key, req.ETag, req.SizeBytes, req.Width, req.Height,
	).Scan(&artifactID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	// Bump status from earlier states ('draft','editing','encoding','uploading')
	// to 'ready' once an artifact is registered. 'sent' / 'delivered' are
	// later phases — don't regress those.
	var newStatus string
	err = h.DB.QueryRow(ctx, `
		UPDATE jumps
		SET status = 'ready'
		WHERE id = $1
		  AND status IN ('draft','editing','encoding','uploading')
		RETURNING status`,
		jumpID,
	).Scan(&newStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		// Status was already at 'ready' / 'sent' / etc — read it back for the response.
		_ = h.DB.QueryRow(ctx, `SELECT status FROM jumps WHERE id = $1`, jumpID).Scan(&newStatus)
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, v1.ArtifactRegisterResponse{
		ArtifactID: artifactID,
		JumpStatus: newStatus,
	})
}

// extensionFor maps an artifact kind to a default file extension. Keeps S3
// keys self-describing so an admin browsing the bucket sees something
// usable instead of an opaque blob.
func extensionFor(kind string) string {
	switch kind {
	case "photo", "screenshot":
		return ".jpg"
	default:
		return ".mp4"
	}
}
