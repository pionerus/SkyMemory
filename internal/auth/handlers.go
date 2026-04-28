package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pionerus/freefall/internal/db"
)

// Handlers wires DB-backed auth endpoints. Constructed once at startup.
type Handlers struct {
	DB       *db.Pool
	Sessions *Manager
}

// =====================================================================
// POST /auth/signup
// Creates a tenant + first owner. JSON in/out.
// =====================================================================
type SignupRequest struct {
	TenantName string `json:"tenant_name"` // human name, e.g. "Skydive Tallinn"
	TenantSlug string `json:"tenant_slug"` // URL-safe, e.g. "skydive-tallinn"
	Email      string `json:"email"`
	Password   string `json:"password"`
}

type SignupResponse struct {
	OperatorID int64  `json:"operator_id"`
	TenantID   int64  `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Email      string `json:"email"`
	Role       string `json:"role"`
}

func (h *Handlers) Signup(w http.ResponseWriter, r *http.Request) {
	var req SignupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Could not parse request body.")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.TenantSlug = strings.TrimSpace(strings.ToLower(req.TenantSlug))
	req.TenantName = strings.TrimSpace(req.TenantName)

	if !validEmail(req.Email) {
		writeError(w, http.StatusBadRequest, "INVALID_EMAIL", "Email is not valid.")
		return
	}
	if !validSlug(req.TenantSlug) {
		writeError(w, http.StatusBadRequest, "INVALID_SLUG", "Tenant slug must be 3-40 chars, lowercase letters/digits/dashes.")
		return
	}
	if req.TenantName == "" || len(req.TenantName) > 80 {
		writeError(w, http.StatusBadRequest, "INVALID_NAME", "Tenant name must be 1-80 chars.")
		return
	}
	if len(req.Password) < 10 {
		writeError(w, http.StatusBadRequest, "WEAK_PASSWORD", "Password must be at least 10 characters.")
		return
	}

	pwHash, err := HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "WEAK_PASSWORD", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var tenantID, operatorID int64
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ($1, $2) RETURNING id`,
		req.TenantSlug, req.TenantName,
	).Scan(&tenantID)
	if err != nil {
		if isUniqueViolation(err, "tenants_slug_key") {
			writeError(w, http.StatusConflict, "SLUG_TAKEN", "Tenant slug is already in use.")
			return
		}
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO operators (tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'owner')
		 RETURNING id`,
		tenantID, req.Email, pwHash,
	).Scan(&operatorID)
	if err != nil {
		if isUniqueViolation(err, "operators_tenant_id_email_key") {
			writeError(w, http.StatusConflict, "EMAIL_TAKEN", "An operator with that email already exists in this tenant.")
			return
		}
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	// Auto-login on signup. Subsequent calls are authenticated.
	if err := h.Sessions.Set(w, r, SessionData{
		OperatorID:    operatorID,
		TenantID:      tenantID,
		OperatorRole:  "owner",
		OperatorEmail: req.Email,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "SESSION_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, SignupResponse{
		OperatorID: operatorID,
		TenantID:   tenantID,
		TenantSlug: req.TenantSlug,
		Email:      req.Email,
		Role:       "owner",
	})
}

// =====================================================================
// POST /auth/login
// =====================================================================
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// Optional: if multiple tenants share an email, require slug to disambiguate.
	TenantSlug string `json:"tenant_slug,omitempty"`
}

type LoginResponse struct {
	OperatorID int64  `json:"operator_id"`
	TenantID   int64  `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Email      string `json:"email"`
	Role       string `json:"role"`
}

func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Could not parse request body.")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.TenantSlug = strings.TrimSpace(strings.ToLower(req.TenantSlug))

	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELDS", "Email and password are required.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Look up the operator. If tenant_slug is provided, scope by it.
	// Without a slug we pick the most-recent operator with that email — works while
	// we have at most one tenant per email globally; we'll add a UNIQUE(email)
	// constraint later or always require slug in production.
	var (
		operatorID   int64
		tenantID     int64
		passwordHash string
		role         string
		tenantSlug   string
	)

	if req.TenantSlug != "" {
		err := h.DB.QueryRow(ctx,
			`SELECT o.id, o.tenant_id, o.password_hash, o.role, t.slug
			 FROM operators o
			 JOIN tenants t ON t.id = o.tenant_id
			 WHERE o.email = $1 AND t.slug = $2 AND t.deleted_at IS NULL`,
			req.Email, req.TenantSlug,
		).Scan(&operatorID, &tenantID, &passwordHash, &role, &tenantSlug)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is incorrect.")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
	} else {
		// Pick the single tenant for this email; if more than one, demand a slug.
		rows, err := h.DB.Query(ctx,
			`SELECT o.id, o.tenant_id, o.password_hash, o.role, t.slug
			 FROM operators o
			 JOIN tenants t ON t.id = o.tenant_id
			 WHERE o.email = $1 AND t.deleted_at IS NULL
			 ORDER BY o.id ASC LIMIT 2`,
			req.Email,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		var found []struct {
			OperatorID   int64
			TenantID     int64
			PasswordHash string
			Role         string
			Slug         string
		}
		for rows.Next() {
			var rec struct {
				OperatorID   int64
				TenantID     int64
				PasswordHash string
				Role         string
				Slug         string
			}
			if err := rows.Scan(&rec.OperatorID, &rec.TenantID, &rec.PasswordHash, &rec.Role, &rec.Slug); err != nil {
				rows.Close()
				writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
				return
			}
			found = append(found, rec)
		}
		rows.Close()
		if len(found) == 0 {
			writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is incorrect.")
			return
		}
		if len(found) > 1 {
			writeError(w, http.StatusConflict, "TENANT_SLUG_REQUIRED", "Multiple clubs share this email — include tenant_slug.")
			return
		}
		operatorID = found[0].OperatorID
		tenantID = found[0].TenantID
		passwordHash = found[0].PasswordHash
		role = found[0].Role
		tenantSlug = found[0].Slug
	}

	if !VerifyPassword(passwordHash, req.Password) {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is incorrect.")
		return
	}

	// Bump last_login_at. Best-effort — login still succeeds even if the UPDATE fails.
	_, _ = h.DB.Exec(ctx, `UPDATE operators SET last_login_at = now() WHERE id = $1`, operatorID)

	if err := h.Sessions.Set(w, r, SessionData{
		OperatorID:    operatorID,
		TenantID:      tenantID,
		OperatorRole:  role,
		OperatorEmail: req.Email,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "SESSION_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, LoginResponse{
		OperatorID: operatorID,
		TenantID:   tenantID,
		TenantSlug: tenantSlug,
		Email:      req.Email,
		Role:       role,
	})
}

// =====================================================================
// POST /auth/logout
// =====================================================================
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	_ = h.Sessions.Clear(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// =====================================================================
// GET /auth/me
// =====================================================================
type MeResponse struct {
	OperatorID    int64  `json:"operator_id"`
	TenantID      int64  `json:"tenant_id"`
	TenantSlug    string `json:"tenant_slug"`
	TenantName    string `json:"tenant_name"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	LastLoginAt   *time.Time `json:"last_login_at,omitempty"`
}

func (h *Handlers) Me(w http.ResponseWriter, r *http.Request) {
	s := MustFromContext(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var resp MeResponse
	resp.OperatorID = s.OperatorID
	resp.TenantID = s.TenantID
	resp.Email = s.OperatorEmail
	resp.Role = s.OperatorRole

	err := h.DB.QueryRow(ctx,
		`SELECT t.slug, t.name, o.last_login_at
		 FROM operators o
		 JOIN tenants t ON t.id = o.tenant_id
		 WHERE o.id = $1 AND t.id = $2`,
		s.OperatorID, s.TenantID,
	).Scan(&resp.TenantSlug, &resp.TenantName, &resp.LastLoginAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// =====================================================================
// helpers
// =====================================================================
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

var (
	emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
	slugRe  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,38}[a-z0-9])$`)
)

func validEmail(s string) bool {
	if len(s) > 254 {
		return false
	}
	return emailRe.MatchString(s)
}

func validSlug(s string) bool {
	return slugRe.MatchString(s)
}

// isUniqueViolation checks if err is a Postgres 23505 unique_violation,
// optionally matching the constraint name. Done by string-matching the
// pgconn error rather than depending on pgconn types directly.
func isUniqueViolation(err error, constraintHint string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "23505") && !strings.Contains(msg, "duplicate key") {
		return false
	}
	if constraintHint == "" {
		return true
	}
	return strings.Contains(msg, constraintHint)
}
