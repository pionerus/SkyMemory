package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	v1 "github.com/pionerus/freefall/internal/api/v1"
)

// =====================================================================
// POST /admin/license-tokens
// Owner-only. Creates a fresh token for installing on an operator's studio.
// Returns the plaintext token ONCE — server only stores the hash.
// =====================================================================
type CreateTokenRequest struct {
	Label             string `json:"label,omitempty"`              // human-readable, e.g. "Mac mini in editing booth"
	OperatorID        int64  `json:"operator_id"`                  // tenant-scoped operator this token belongs to
	DeviceFingerprint string `json:"device_fingerprint,omitempty"` // optional, set on first /license/validate
}

type CreateTokenResponse struct {
	ID         int64  `json:"id"`
	Token      string `json:"token"` // plaintext — shown ONCE
	Label      string `json:"label,omitempty"`
	OperatorID int64  `json:"operator_id"`
	CreatedAt  time.Time `json:"created_at"`
}

func (h *Handlers) CreateToken(w http.ResponseWriter, r *http.Request) {
	s := MustFromContext(r.Context())

	var req CreateTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Could not parse request body.")
		return
	}
	if req.OperatorID == 0 {
		req.OperatorID = s.OperatorID // default: token belongs to the calling owner
	}
	if len(req.Label) > 80 {
		writeError(w, http.StatusBadRequest, "LABEL_TOO_LONG", "Label must be 80 chars or fewer.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Verify the target operator belongs to this tenant. Cross-tenant leak guard.
	var ownerExists bool
	err := h.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM operators WHERE id = $1 AND tenant_id = $2)`,
		req.OperatorID, s.TenantID,
	).Scan(&ownerExists)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if !ownerExists {
		writeError(w, http.StatusBadRequest, "INVALID_OPERATOR", "Operator does not belong to this tenant.")
		return
	}

	plaintext, hash, err := GenerateLicenseToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RAND_ERROR", err.Error())
		return
	}

	var (
		id        int64
		createdAt time.Time
	)
	err = h.DB.QueryRow(ctx,
		`INSERT INTO license_tokens (operator_id, tenant_id, token_hash, device_fingerprint, label)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''))
		 RETURNING id, created_at`,
		req.OperatorID, s.TenantID, hash, req.DeviceFingerprint, req.Label,
	).Scan(&id, &createdAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, CreateTokenResponse{
		ID:         id,
		Token:      plaintext,
		Label:      req.Label,
		OperatorID: req.OperatorID,
		CreatedAt:  createdAt,
	})
}

// =====================================================================
// GET /admin/license-tokens
// Lists tokens for the calling tenant. Plaintext is NEVER returned.
// =====================================================================
type ListedToken struct {
	ID                int64      `json:"id"`
	OperatorID        int64      `json:"operator_id"`
	OperatorEmail     string     `json:"operator_email"`
	Label             string     `json:"label,omitempty"`
	DeviceFingerprint string     `json:"device_fingerprint,omitempty"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP        string     `json:"last_used_ip,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
}

type ListTokensResponse struct {
	Tokens []ListedToken `json:"tokens"`
}

func (h *Handlers) ListTokens(w http.ResponseWriter, r *http.Request) {
	s := MustFromContext(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.DB.Query(ctx,
		`SELECT lt.id, lt.operator_id, o.email, COALESCE(lt.label, ''),
		        COALESCE(lt.device_fingerprint, ''),
		        lt.last_used_at,
		        host(lt.last_used_ip) AS last_used_ip,
		        lt.created_at, lt.revoked_at
		 FROM license_tokens lt
		 JOIN operators o ON o.id = lt.operator_id
		 WHERE lt.tenant_id = $1
		 ORDER BY lt.created_at DESC`,
		s.TenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	defer rows.Close()

	out := []ListedToken{}
	for rows.Next() {
		var t ListedToken
		var ip *string
		if err := rows.Scan(&t.ID, &t.OperatorID, &t.OperatorEmail, &t.Label,
			&t.DeviceFingerprint, &t.LastUsedAt, &ip,
			&t.CreatedAt, &t.RevokedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		if ip != nil {
			t.LastUsedIP = *ip
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, ListTokensResponse{Tokens: out})
}

// =====================================================================
// DELETE /admin/license-tokens/:id
// Soft-revoke: sets revoked_at = now(). The row stays so audit trail
// (last_used_*) survives.
// =====================================================================
func (h *Handlers) RevokeToken(w http.ResponseWriter, r *http.Request) {
	s := MustFromContext(r.Context())

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Token id must be an integer.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	tag, err := h.DB.Exec(ctx,
		`UPDATE license_tokens SET revoked_at = now()
		 WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL`,
		id, s.TenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Token not found or already revoked.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// =====================================================================
// POST /api/v1/license/validate
// Studio calls this at startup + every 24h. No session — auth is the token in
// the request body itself. Returns tenant info + valid_until.
// =====================================================================
func (h *Handlers) ValidateLicense(w http.ResponseWriter, r *http.Request) {
	var req v1.LicenseValidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, v1.LicenseValidateResponse{
			Valid: false, Reason: "invalid_json",
		})
		return
	}
	if req.Token == "" {
		writeJSON(w, http.StatusOK, v1.LicenseValidateResponse{
			Valid: false, Reason: "token_missing",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	hash := HashLicenseToken(req.Token)

	var (
		tokenID       int64
		operatorID    int64
		tenantID      int64
		operatorEmail string
		tenantName    string
		revoked       *time.Time
		tenantDeleted *time.Time
	)
	err := h.DB.QueryRow(ctx,
		`SELECT lt.id, lt.operator_id, lt.tenant_id, lt.revoked_at,
		        o.email, t.name, t.deleted_at
		 FROM license_tokens lt
		 JOIN operators o ON o.id = lt.operator_id
		 JOIN tenants t ON t.id = lt.tenant_id
		 WHERE lt.token_hash = $1`,
		hash,
	).Scan(&tokenID, &operatorID, &tenantID, &revoked, &operatorEmail, &tenantName, &tenantDeleted)

	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, v1.LicenseValidateResponse{
			Valid: false, Reason: "token_unknown",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, v1.LicenseValidateResponse{
			Valid: false, Reason: "db_error",
		})
		return
	}
	if revoked != nil {
		writeJSON(w, http.StatusOK, v1.LicenseValidateResponse{
			Valid: false, Reason: "token_revoked",
		})
		return
	}
	if tenantDeleted != nil {
		writeJSON(w, http.StatusOK, v1.LicenseValidateResponse{
			Valid: false, Reason: "tenant_deleted",
		})
		return
	}

	// Update last_used_at + last_used_ip + maybe device_fingerprint. Best-effort.
	go func(token int64, ip string, fp string) {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer bgCancel()
		_, _ = h.DB.Exec(bgCtx,
			`UPDATE license_tokens
			 SET last_used_at = now(),
			     last_used_ip = NULLIF($1, '')::INET,
			     device_fingerprint = COALESCE(NULLIF($2, ''), device_fingerprint)
			 WHERE id = $3`,
			ip, fp, token,
		)
	}(tokenID, clientIP(r), req.DeviceFingerprint)

	writeJSON(w, http.StatusOK, v1.LicenseValidateResponse{
		Valid:         true,
		TenantID:      tenantID,
		OperatorID:    operatorID,
		TenantName:    tenantName,
		OperatorEmail: operatorEmail,
		ValidUntil:    time.Now().Add(24 * time.Hour),
	})
}

// =====================================================================
// helper: extract the best-guess client IP for audit logging
// =====================================================================
func clientIP(r *http.Request) string {
	// chi/middleware.RealIP already rewrote r.RemoteAddr from X-Forwarded-For when present.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	// Strip IPv6 zone if any.
	if i := strings.Index(host, "%"); i > 0 {
		host = host[:i]
	}
	return host
}
