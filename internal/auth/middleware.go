package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pionerus/freefall/internal/db"
)

// RequireSession blocks unauthenticated requests and stashes SessionData in the request context.
// Returns 401 with JSON {error, code} on failure.
func (m *Manager) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := m.Read(r)
		if !s.IsAuthenticated() {
			writeError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "Sign in to continue.")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), s)))
	})
}

// RequireOwner extends RequireSession to also enforce role='owner'.
// Use for endpoints that mutate billing config, payment keys, storage config, license tokens.
func (m *Manager) RequireOwner(next http.Handler) http.Handler {
	return m.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := MustFromContext(r.Context())
		if !s.IsOwner() {
			writeError(w, http.StatusForbidden, "OWNER_REQUIRED", "Owner role is required.")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// RequirePlatformAdmin blocks anyone but a platform admin. Used for /platform/*
// routes that read cross-tenant data and mutate per-club pricing / app_settings.
// On a browser hit (Accept: text/html) we redirect to /platform/login instead
// of returning JSON 401 so an unauthenticated visit gets a friendly form.
func (m *Manager) RequirePlatformAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := m.Read(r)
		if !s.IsPlatformAdmin() {
			if isHTMLRequest(r) {
				http.Redirect(w, r, "/platform/login", http.StatusFound)
				return
			}
			writeError(w, http.StatusUnauthorized, "PLATFORM_ADMIN_REQUIRED", "Platform admin sign-in is required.")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), s)))
	})
}

// isHTMLRequest checks Accept header for "text/html" precedence over JSON. Keeps
// browser requests routed to redirects, API requests to JSON 401.
func isHTMLRequest(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	for _, part := range []string{"text/html", "application/xhtml"} {
		if len(accept) >= len(part) && containsAccept(accept, part) {
			return true
		}
	}
	return false
}

func containsAccept(haystack, needle string) bool {
	// Plain substring search; mime-type lists never contain weird escape chars
	// so this is safe and avoids importing mime parsing.
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// RequireLicenseToken authenticates a request via "Authorization: Bearer <token>".
// Used for studio-facing endpoints (jumps/register, music/suggest, upload-init, …).
// On success, stashes SessionData in the request context like RequireSession does,
// so handlers can use the same MustFromContext / FromContext helpers.
func RequireLicenseToken(pool *db.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			plaintext, err := ParseLicenseTokenFromHeader(r.Header.Get("Authorization"))
			if err != nil {
				writeError(w, http.StatusUnauthorized, "AUTH_REQUIRED", err.Error())
				return
			}
			hash := HashLicenseToken(plaintext)

			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()

			var (
				operatorID    int64
				tenantID      int64
				role          string
				operatorEmail string
				revoked       *time.Time
				tenantDeleted *time.Time
			)
			err = pool.QueryRow(ctx,
				`SELECT lt.operator_id, lt.tenant_id, lt.revoked_at,
				        o.role, o.email, t.deleted_at
				 FROM license_tokens lt
				 JOIN operators o ON o.id = lt.operator_id
				 JOIN tenants t ON t.id = lt.tenant_id
				 WHERE lt.token_hash = $1`,
				hash,
			).Scan(&operatorID, &tenantID, &revoked, &role, &operatorEmail, &tenantDeleted)

			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusUnauthorized, "TOKEN_INVALID", "Token not recognized.")
				return
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
				return
			}
			if revoked != nil {
				writeError(w, http.StatusUnauthorized, "TOKEN_REVOKED", "Token has been revoked.")
				return
			}
			if tenantDeleted != nil {
				writeError(w, http.StatusUnauthorized, "TENANT_DELETED", "Tenant is no longer active.")
				return
			}

			s := SessionData{
				OperatorID:    operatorID,
				TenantID:      tenantID,
				OperatorRole:  role,
				OperatorEmail: operatorEmail,
			}
			next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), s)))
		})
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":    code,
		"message": message,
	})
}
