package auth

import (
	"encoding/json"
	"net/http"
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

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":    code,
		"message": message,
	})
}
