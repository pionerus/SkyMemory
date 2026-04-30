package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// =====================================================================
// POST /platform/login
// Authenticates a row in platform_admins. Issues a session that's
// mutually exclusive with operator/tenant sessions (Set vs SetPlatformAdmin
// each clear the other side's keys).
// =====================================================================

type PlatformLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type PlatformLoginResponse struct {
	AdminID int64  `json:"admin_id"`
	Email   string `json:"email"`
	Name    string `json:"name"`
}

func (h *Handlers) PlatformLogin(w http.ResponseWriter, r *http.Request) {
	var req PlatformLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Could not parse request body.")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELDS", "Email and password are required.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var (
		id           int64
		name         string
		passwordHash string
	)
	err := h.DB.QueryRow(ctx,
		`SELECT id, name, password_hash
		 FROM platform_admins
		 WHERE email = $1 AND deleted_at IS NULL`,
		req.Email,
	).Scan(&id, &name, &passwordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is incorrect.")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	if !VerifyPassword(passwordHash, req.Password) {
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is incorrect.")
		return
	}

	_, _ = h.DB.Exec(ctx, `UPDATE platform_admins SET last_login_at = now() WHERE id = $1`, id)

	if err := h.Sessions.SetPlatformAdmin(w, r, id, name, req.Email); err != nil {
		writeError(w, http.StatusInternalServerError, "SESSION_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, PlatformLoginResponse{
		AdminID: id,
		Email:   req.Email,
		Name:    name,
	})
}

// PlatformLogout reuses the regular session-clear logic since both identity
// types share the same cookie. Kept as a separate route so /platform/* has
// its own logout endpoint that admins can wire to a button.
func (h *Handlers) PlatformLogout(w http.ResponseWriter, r *http.Request) {
	if err := h.Sessions.Clear(w, r); err != nil {
		writeError(w, http.StatusInternalServerError, "SESSION_ERROR", err.Error())
		return
	}
	if isHTMLRequest(r) {
		http.Redirect(w, r, "/platform/login", http.StatusFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
