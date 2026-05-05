package drive

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pionerus/freefall/internal/auth"
	v1 "github.com/pionerus/freefall/internal/api/v1"
	"github.com/pionerus/freefall/internal/secrets"
)

// Handlers wires the /operator/storage + /auth/google-drive/* endpoints.
// Renderer is the cloud's html/template — passed in to avoid an import
// cycle from web/server/templates.
type Handlers struct {
	Client    *Client
	Templates Renderer
}

type Renderer interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}

// PageData is what operator_storage.html receives.
type PageData struct {
	Active        string
	OperatorEmail string
	OperatorRole  string
	TenantName    string

	Configured bool       // server has GOOGLE_OAUTH_CLIENT_ID etc.
	Connected  bool       // operator has a row in operator_drive_configs
	Config     *ConfigRow // populated when Connected
	FlashOK    string
	FlashError string
}

const stateCookieName = "drive_oauth_state"

// renderPage is the GET /operator/storage handler.
func (h *Handlers) renderPage(w http.ResponseWriter, r *http.Request, pd PageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.Templates.ExecuteTemplate(w, "operator_storage.html", pd); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Page renders /operator/storage. Operator must be signed in (RequireSession
// middleware enforces this; we just pull the session here for ID + email).
func (h *Handlers) Page(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	pd := PageData{
		Active:        "storage",
		OperatorEmail: s.OperatorEmail,
		OperatorRole:  s.OperatorRole,
		Configured:    h.Client.Configured(),
		FlashOK:       r.URL.Query().Get("ok"),
		FlashError:    r.URL.Query().Get("error"),
	}
	if cfg, err := h.Client.GetConfig(r.Context(), s.OperatorID); err == nil {
		pd.Connected = true
		pd.Config = cfg
	} else if !errors.Is(err, ErrNotConfigured) {
		pd.FlashError = err.Error()
	}
	h.renderPage(w, r, pd)
}

// Start kicks off the OAuth dance — generates a random state, sets it as
// a cookie (for CSRF), redirects to Google. Returns 503 with a friendly
// banner when the server doesn't have ClientID/Secret loaded.
func (h *Handlers) Start(w http.ResponseWriter, r *http.Request) {
	if !h.Client.Configured() {
		http.Redirect(w, r, "/operator/storage?error=not_configured", http.StatusSeeOther)
		return
	}
	state := randomState()
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/auth/google-drive",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode, // Google's redirect crosses site
		Secure:   r.TLS != nil,
		MaxAge:   600,
	})
	http.Redirect(w, r, h.Client.AuthorizeURL(state), http.StatusSeeOther)
}

// Callback handles Google's OAuth redirect. Exchanges the code, encrypts
// the refresh token, persists the row, ensures the root folder exists,
// then redirects to /operator/storage with a success flash.
func (h *Handlers) Callback(w http.ResponseWriter, r *http.Request, masterKey []byte) {
	// CSRF check.
	gotState := r.URL.Query().Get("state")
	c, err := r.Cookie(stateCookieName)
	if err != nil || gotState == "" || gotState != c.Value {
		http.Redirect(w, r, "/operator/storage?error=state_mismatch", http.StatusSeeOther)
		return
	}
	// Operator session is required — Callback is mounted behind RequireSession.
	s := auth.MustFromContext(r.Context())

	if errCode := r.URL.Query().Get("error"); errCode != "" {
		http.Redirect(w, r, "/operator/storage?error="+errCode, http.StatusSeeOther)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/operator/storage?error=no_code", http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	res, err := h.Client.ExchangeCode(ctx, code)
	if err != nil {
		http.Redirect(w, r, "/operator/storage?error="+httpEncode(err.Error()), http.StatusSeeOther)
		return
	}

	enc, err := secrets.Encrypt(masterKey, []byte(res.RefreshToken))
	if err != nil {
		http.Redirect(w, r, "/operator/storage?error=encrypt_failed", http.StatusSeeOther)
		return
	}

	row := ConfigRow{
		OperatorID:         s.OperatorID,
		TenantID:           s.TenantID,
		GoogleAccountEmail: res.UserEmail,
		GoogleAccountID:    res.UserSubject,
		RefreshTokenEnc:    enc,
		Scopes:             res.GrantedScopes,
	}
	if err := h.Client.UpsertConfig(ctx, row); err != nil {
		http.Redirect(w, r, "/operator/storage?error=save_failed", http.StatusSeeOther)
		return
	}

	// Bootstrap the root folder. New connect = no cached id; we just
	// create. Failure here doesn't break the connect — next upload (or
	// "Test connection") will retry.
	if folderID, ferr := h.Client.EnsureRootFolder(ctx, res.AccessToken, ""); ferr == nil {
		_ = h.Client.SaveRootFolderID(ctx, s.OperatorID, folderID)
	}
	_ = h.Client.SaveAccessTokenCache(ctx, s.OperatorID, res.AccessToken, res.AccessExpires)

	http.Redirect(w, r, "/operator/storage?ok=connected", http.StatusSeeOther)
}

// Disconnect revokes the stored refresh token at Google's end and deletes
// the row. Past artifacts on Drive stay (operator owns them).
func (h *Handlers) Disconnect(w http.ResponseWriter, r *http.Request, masterKey []byte) {
	s := auth.MustFromContext(r.Context())
	cfg, err := h.Client.GetConfig(r.Context(), s.OperatorID)
	if err != nil {
		http.Redirect(w, r, "/operator/storage?error=not_connected", http.StatusSeeOther)
		return
	}
	rt, derr := secrets.Decrypt(masterKey, cfg.RefreshTokenEnc)
	if derr == nil {
		// Best-effort revoke — even if Google says no, we delete locally.
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		_ = h.Client.Revoke(ctx, string(rt))
		cancel()
	}
	if err := h.Client.Delete(r.Context(), s.OperatorID); err != nil {
		http.Redirect(w, r, "/operator/storage?error=delete_failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/operator/storage?ok=disconnected", http.StatusSeeOther)
}

// Test runs a quick connectivity probe — refresh the access token, make
// sure the root folder is still there, write the result to last_health_*.
func (h *Handlers) Test(w http.ResponseWriter, r *http.Request, masterKey []byte) {
	s := auth.MustFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	cfg, err := h.Client.GetConfig(ctx, s.OperatorID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "not_connected"})
		return
	}
	rt, err := secrets.Decrypt(masterKey, cfg.RefreshTokenEnc)
	if err != nil {
		_ = h.Client.SaveHealth(ctx, s.OperatorID, false, "decrypt: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "decrypt"})
		return
	}
	at, expires, err := h.Client.RefreshAccessToken(ctx, string(rt))
	if err != nil {
		_ = h.Client.SaveHealth(ctx, s.OperatorID, false, "refresh: "+err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = h.Client.SaveAccessTokenCache(ctx, s.OperatorID, at, expires)

	// Touch the root folder — verifies cached id exists, creates one if
	// the operator deleted it from Drive (or this is a first-test after
	// a partial connect that didn't persist root_folder_id).
	folderID, err := h.Client.EnsureRootFolder(ctx, at, cfg.RootFolderID)
	if err != nil {
		_ = h.Client.SaveHealth(ctx, s.OperatorID, false, "ensure_root: "+err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = h.Client.SaveRootFolderID(ctx, s.OperatorID, folderID)
	_ = h.Client.SaveHealth(ctx, s.OperatorID, true, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"root_folder_id": folderID,
		"email":          cfg.GoogleAccountEmail,
	})
}

// UploadToken handles GET /api/v1/jumps/{id}/drive-token.
// Studio calls this before uploading a rendered artifact. If the operator has
// Drive connected we return a short-lived access token + the per-jump folder id.
// Studio uses these to push directly to the Drive API (resumable upload).
func (h *Handlers) UploadToken(w http.ResponseWriter, r *http.Request, masterKey []byte) {
	s := auth.MustFromContext(r.Context())

	jumpID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || jumpID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	cfg, err := h.Client.GetConfig(ctx, s.OperatorID)
	if errors.Is(err, ErrNotConfigured) {
		writeJSON(w, http.StatusOK, v1.DriveUploadTokenResponse{Connected: false})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Decrypt stored refresh token.
	rt, err := secrets.Decrypt(masterKey, cfg.RefreshTokenEnc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "decrypt"})
		return
	}

	// Refresh access token if expiring within 2 minutes.
	accessToken := cfg.AccessTokenCache
	if accessToken == "" || time.Now().After(cfg.AccessTokenExpiresAt.Add(-2*time.Minute)) {
		at, expires, rerr := h.Client.RefreshAccessToken(ctx, string(rt))
		if rerr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": rerr.Error()})
			return
		}
		accessToken = at
		_ = h.Client.SaveAccessTokenCache(ctx, s.OperatorID, at, expires)
	}

	// Get or create the per-jump Drive folder.
	folderID, err := h.Client.GetOrCreateJumpFolder(ctx, accessToken, s.OperatorID, jumpID, cfg.RootFolderID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, v1.DriveUploadTokenResponse{
		Connected:   true,
		AccessToken: accessToken,
		FolderID:    folderID,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomState() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// httpEncode is a tiny URL-query escape that keeps the redirect message
// readable when displayed back on /operator/storage.
func httpEncode(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' || c == ' '
		if ok {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return string(out)
}
