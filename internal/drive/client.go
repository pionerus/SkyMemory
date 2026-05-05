// Package drive integrates Google Drive as a per-operator delivery
// destination. Each operator authenticates via OAuth on /operator/storage;
// we keep their refresh token encrypted in operator_drive_configs and use
// it to mint short-lived access tokens for studio uploads + watch-page
// share links.
//
// Phase A: package skeleton + token refresh + whoami. Upload, folder
// creation, and permission setting land in Phase B/C.
package drive

import (
	"context"
	"net/http"
	"time"

	"github.com/pionerus/freefall/internal/db"
)

// Config carries everything the package needs at boot. ClientID + ClientSecret
// come from .env (or app_settings later); RedirectURL is built from the
// canonical cloud base URL.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string // e.g. https://api.skydivememory.app/auth/google-drive/callback
	// Scopes granted on the consent screen. Phase A asks for `drive.file`
	// (only files we create) plus `userinfo.email` to label the operator's
	// connection in the UI.
	Scopes []string
}

// DefaultScopes returns the minimal set we need: file-scoped Drive +
// userinfo for "connected as ..." display.
func DefaultScopes() []string {
	return []string{
		"https://www.googleapis.com/auth/drive.file",
		"https://www.googleapis.com/auth/userinfo.email",
		"openid",
	}
}

// Client is the cloud-side Drive integration. Holds the OAuth config + DB
// pool; constructed once at boot and shared by every handler.
type Client struct {
	cfg  Config
	db   *db.Pool
	http *http.Client
}

// New builds a Client. The HTTP client uses a 30s timeout — enough for
// userinfo + token endpoints; resumable upload code path uses its own
// longer-timeout client.
func New(cfg Config, pool *db.Pool) *Client {
	return &Client{
		cfg:  cfg,
		db:   pool,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Configured reports whether ClientID/Secret/RedirectURL are populated.
// Handlers use this to short-circuit with a "set GOOGLE_OAUTH_CLIENT_ID"
// banner instead of returning a confusing OAuth error.
func (c *Client) Configured() bool {
	return c.cfg.ClientID != "" && c.cfg.ClientSecret != "" && c.cfg.RedirectURL != ""
}

// reserve unused import — context is used by methods landing in next phase
var _ = context.Background
