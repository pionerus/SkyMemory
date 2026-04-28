package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/gorilla/sessions"
)

const (
	sessionName       = "freefall_session"
	keyOperatorID     = "operator_id"
	keyTenantID       = "tenant_id"
	keyOperatorRole   = "operator_role"
	keyOperatorEmail  = "operator_email"
)

// Manager wraps a sessions.Store with helpers for the operator session.
// In dev: cookie-backed via NewCookieStore. In prod: same — this app is single-instance
// for the foreseeable future. Switching to Redis-backed sessions later is a Store swap.
type Manager struct {
	store sessions.Store
	prod  bool
}

func NewManager(secretKey string, productionMode bool) *Manager {
	store := sessions.NewCookieStore([]byte(secretKey))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 14, // 14 days
		HttpOnly: true,
		Secure:   productionMode, // dev runs on http://localhost
		SameSite: http.SameSiteStrictMode,
	}
	return &Manager{store: store, prod: productionMode}
}

// SessionData is the typed view of a session.
type SessionData struct {
	OperatorID    int64
	TenantID      int64
	OperatorRole  string
	OperatorEmail string
}

func (s SessionData) IsAuthenticated() bool { return s.OperatorID > 0 && s.TenantID > 0 }
func (s SessionData) IsOwner() bool         { return s.OperatorRole == "owner" }

// Read returns the typed session for a request. Always non-nil; check IsAuthenticated.
func (m *Manager) Read(r *http.Request) SessionData {
	sess, err := m.store.Get(r, sessionName)
	if err != nil || sess == nil {
		return SessionData{}
	}
	return SessionData{
		OperatorID:    int64Of(sess.Values[keyOperatorID]),
		TenantID:      int64Of(sess.Values[keyTenantID]),
		OperatorRole:  stringOf(sess.Values[keyOperatorRole]),
		OperatorEmail: stringOf(sess.Values[keyOperatorEmail]),
	}
}

// Set writes the operator's identity into the session and saves the cookie.
func (m *Manager) Set(w http.ResponseWriter, r *http.Request, d SessionData) error {
	sess, err := m.store.Get(r, sessionName)
	if err != nil {
		return err
	}
	sess.Values[keyOperatorID] = d.OperatorID
	sess.Values[keyTenantID] = d.TenantID
	sess.Values[keyOperatorRole] = d.OperatorRole
	sess.Values[keyOperatorEmail] = d.OperatorEmail
	return sess.Save(r, w)
}

// Clear logs out — empties the session values and sets MaxAge<0 to evict the cookie.
func (m *Manager) Clear(w http.ResponseWriter, r *http.Request) error {
	sess, err := m.store.Get(r, sessionName)
	if err != nil {
		return err
	}
	sess.Options.MaxAge = -1
	for k := range sess.Values {
		delete(sess.Values, k)
	}
	return sess.Save(r, w)
}

// =====================================================================
// Context helpers — middleware stashes SessionData here for handlers.
// =====================================================================
type ctxKey int

const (
	ctxKeySession ctxKey = iota
)

func WithSession(ctx context.Context, s SessionData) context.Context {
	return context.WithValue(ctx, ctxKeySession, s)
}

func FromContext(ctx context.Context) (SessionData, bool) {
	s, ok := ctx.Value(ctxKeySession).(SessionData)
	return s, ok
}

// MustFromContext panics if no session in ctx — use only in handlers behind RequireSession.
func MustFromContext(ctx context.Context) SessionData {
	s, ok := FromContext(ctx)
	if !ok {
		panic(errors.New("auth.MustFromContext: no session in context — handler not behind RequireSession"))
	}
	return s
}

// =====================================================================
// internal helpers
// =====================================================================
func int64Of(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
