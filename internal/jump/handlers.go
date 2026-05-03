package jump

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	v1 "github.com/pionerus/freefall/internal/api/v1"
	"github.com/pionerus/freefall/internal/auth"
	"github.com/pionerus/freefall/internal/db"
)

// Handlers wires the jump-record endpoints under /api/v1/jumps/*.
// All endpoints here expect to be mounted behind auth.RequireLicenseToken so
// SessionData is in context (operator_id, tenant_id, role).
type Handlers struct {
	DB *db.Pool
}

// =====================================================================
// POST /api/v1/jumps/register
// Studio creates the cloud-side client + jump record before any uploads happen.
// Returns jump_id + access_code so studio can stash them in local SQLite.
//
// Idempotency: NOT idempotent. Each call creates a new client + jump. The studio
// is expected to call this exactly once when the operator clicks "New project".
// =====================================================================
func (h *Handlers) Register(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	var req v1.JumpRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Could not parse request body.")
		return
	}

	req.ClientName = strings.TrimSpace(req.ClientName)
	req.ClientEmail = strings.TrimSpace(strings.ToLower(req.ClientEmail))
	req.ClientPhone = strings.TrimSpace(req.ClientPhone)

	// When picking an existing client (assigned by club admin), name comes
	// from the DB row — only walk-ins need a typed name on the request.
	if req.ExistingClientID == 0 && (req.ClientName == "" || len(req.ClientName) > 200) {
		writeError(w, http.StatusBadRequest, "INVALID_CLIENT_NAME", "Client name is required and must be ≤200 chars.")
		return
	}
	if !req.Output1080p && !req.Output4K && !req.OutputVertical && !req.OutputPhotos {
		writeError(w, http.StatusBadRequest, "NO_OUTPUTS", "At least one output format must be selected.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	tx, err := h.DB.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		clientID         int64
		canonical, formatted string
	)

	if req.ExistingClientID > 0 {
		// Existing client — pull their access_code so studio can stash it
		// alongside the jump locally. Tenant scoping prevents cross-tenant ID guesses.
		err = tx.QueryRow(ctx,
			`SELECT id, access_code FROM clients
			 WHERE id = $1 AND tenant_id = $2`,
			req.ExistingClientID, s.TenantID,
		).Scan(&clientID, &canonical)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "CLIENT_NOT_FOUND", "Client not found in your tenant.")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		if len(canonical) == 8 {
			formatted = canonical[:4] + "-" + canonical[4:]
		} else {
			formatted = canonical
		}
	} else {
		// Walk-in: insert a new client. UNIQUE on access_code can collide on
		// rare 1-in-10^12 duplicates; retry up to 3 times before giving up.
		const maxAttempts = 3
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			canonical, formatted, err = NewAccessCode()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "RAND_ERROR", err.Error())
				return
			}

			err = tx.QueryRow(ctx,
				`INSERT INTO clients (tenant_id, name, email, phone, access_code, created_by)
				 VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6)
				 RETURNING id`,
				s.TenantID, req.ClientName, req.ClientEmail, req.ClientPhone, canonical, s.OperatorID,
			).Scan(&clientID)

			if err == nil {
				break // got a unique access_code
			}
			if isUniqueViolation(err, "clients_access_code_key") && attempt < maxAttempts {
				continue // try again with a new random code
			}
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
	}

	// Snapshot tenant.photo_pack_price_cents at jump-create time so subsequent
	// admin-side price changes don't retroactively bill differently.
	var photoPackPriceSnapshot int
	if err := tx.QueryRow(ctx,
		`SELECT photo_pack_price_cents FROM tenants WHERE id = $1`,
		s.TenantID,
	).Scan(&photoPackPriceSnapshot); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	var jumpID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO jumps
		   (tenant_id, client_id, operator_id,
		    output_1080p, output_4k, output_vertical, output_photos,
		    photo_pack_price_cents_snapshot,
		    has_operator_uploaded_photos)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id`,
		s.TenantID, clientID, s.OperatorID,
		req.Output1080p, req.Output4K, req.OutputVertical, req.OutputPhotos,
		photoPackPriceSnapshot,
		req.HasOperatorUploadedPhotos,
	).Scan(&jumpID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, v1.JumpRegisterResponse{
		JumpID:     jumpID,
		ClientID:   clientID,
		AccessCode: formatted, // dashed, human-readable
	})
}

// =====================================================================
// PUT /api/v1/jumps/:id/music — set/clear the picked music track for a jump.
// Body: {music_track_id: 42} or {music_track_id: 0} to clear.
// =====================================================================
type SetMusicRequest struct {
	MusicTrackID int64 `json:"music_track_id"`
}

type SetMusicResponse struct {
	JumpID       int64 `json:"jump_id"`
	MusicTrackID int64 `json:"music_track_id"`
}

func (h *Handlers) SetMusic(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	id, err := parseInt64Param(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}

	var req SetMusicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Verify the jump belongs to this tenant before touching it.
	var owned bool
	err = h.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM jumps WHERE id = $1 AND tenant_id = $2)`,
		id, s.TenantID,
	).Scan(&owned)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if !owned {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Jump not found in this tenant.")
		return
	}

	// Verify the requested track is visible to this tenant (NULL = global).
	// 0 means "clear", which we allow without further check.
	if req.MusicTrackID > 0 {
		var visible bool
		err = h.DB.QueryRow(ctx, `
			SELECT EXISTS(
			  SELECT 1 FROM music_visible_to
			  WHERE id = $1 AND (tenant_id IS NULL OR tenant_id = $2)
			)`,
			req.MusicTrackID, s.TenantID,
		).Scan(&visible)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		if !visible {
			writeError(w, http.StatusBadRequest, "INVALID_TRACK", "Track not in your library or inactive.")
			return
		}
	}

	// 0 in incoming JSON means clear — translate to NULL.
	if req.MusicTrackID == 0 {
		_, err = h.DB.Exec(ctx,
			`UPDATE jumps SET music_track_id = NULL WHERE id = $1 AND tenant_id = $2`,
			id, s.TenantID)
	} else {
		_, err = h.DB.Exec(ctx,
			`UPDATE jumps SET music_track_id = $1 WHERE id = $2 AND tenant_id = $3`,
			req.MusicTrackID, id, s.TenantID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, SetMusicResponse{
		JumpID:       id,
		MusicTrackID: req.MusicTrackID,
	})
}

// =====================================================================
// GET /api/v1/jumps/:id  — used by studio to refresh status from cloud
// (e.g. after operator hits "Send to client" the cloud flips status to 'sent')
// =====================================================================
type GetJumpResponse struct {
	JumpID                 int64  `json:"jump_id"`
	ClientID               int64  `json:"client_id"`
	ClientName             string `json:"client_name"`
	AccessCode             string `json:"access_code"`
	Status                 string `json:"status"`
	Output1080p            bool   `json:"output_1080p"`
	Output4K               bool   `json:"output_4k"`
	OutputVertical         bool   `json:"output_vertical"`
	OutputPhotos           bool   `json:"output_photos"`
	PhotoPackUnlocked      bool   `json:"photo_pack_unlocked"`
}

func (h *Handlers) GetByIDForStudio(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	id, err := parseInt64Param(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var resp GetJumpResponse
	err = h.DB.QueryRow(ctx,
		`SELECT j.id, c.id, c.name, c.access_code, j.status,
		        j.output_1080p, j.output_4k, j.output_vertical, j.output_photos,
		        j.photo_pack_unlocked
		 FROM jumps j
		 JOIN clients c ON c.id = j.client_id
		 WHERE j.id = $1 AND j.tenant_id = $2`,
		id, s.TenantID,
	).Scan(
		&resp.JumpID, &resp.ClientID, &resp.ClientName, &resp.AccessCode, &resp.Status,
		&resp.Output1080p, &resp.Output4K, &resp.OutputVertical, &resp.OutputPhotos,
		&resp.PhotoPackUnlocked,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Jump not found in this tenant.")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	// Format access_code with dash for display
	if len(resp.AccessCode) == 8 {
		resp.AccessCode = resp.AccessCode[:4] + "-" + resp.AccessCode[4:]
	}

	writeJSON(w, http.StatusOK, resp)
}
