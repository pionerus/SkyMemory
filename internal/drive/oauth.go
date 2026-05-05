package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AuthorizeURL is what /auth/google-drive/start redirects to. We pin
// `access_type=offline` so Google issues a refresh_token (mandatory for
// our background uploads), and `prompt=consent` so re-connecting an
// already-granted account re-emits the refresh_token (Google omits it on
// silent re-auth otherwise).
func (c *Client) AuthorizeURL(state string) string {
	q := url.Values{
		"client_id":     {c.cfg.ClientID},
		"redirect_uri":  {c.cfg.RedirectURL},
		"response_type": {"code"},
		"scope":         {strings.Join(c.cfg.Scopes, " ")},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode()
}

// tokenResponse is what Google returns from the token + refresh endpoints.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"` // present only on first-grant
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	IDToken      string `json:"id_token,omitempty"`
	Error        string `json:"error,omitempty"`
	ErrorDesc    string `json:"error_description,omitempty"`
}

// ExchangeResult captures everything the callback handler needs to persist.
type ExchangeResult struct {
	AccessToken    string
	AccessExpires  time.Time
	RefreshToken   string
	GrantedScopes  string
	UserEmail      string
	UserSubject    string // stable id from id_token's `sub` — survives email change
}

// ExchangeCode swaps an OAuth `code` for tokens. Called from the callback
// handler. Returns ErrNoRefreshToken when Google didn't send one (usually
// means we forgot prompt=consent or operator already granted previously
// with a different code path).
func (c *Client) ExchangeCode(ctx context.Context, code string) (*ExchangeResult, error) {
	if !c.Configured() {
		return nil, ErrOAuthNotConfigured
	}
	form := url.Values{
		"code":          {code},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"redirect_uri":  {c.cfg.RedirectURL},
		"grant_type":    {"authorization_code"},
	}
	tok, err := c.postToken(ctx, form)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, ErrNoRefreshToken
	}

	res := &ExchangeResult{
		AccessToken:   tok.AccessToken,
		AccessExpires: time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second),
		RefreshToken:  tok.RefreshToken,
		GrantedScopes: tok.Scope,
	}

	// Pull the email + sub from userinfo (cheaper than parsing id_token here
	// and we don't pin a JWT lib for one claim).
	if email, sub, err := c.UserInfo(ctx, tok.AccessToken); err == nil {
		res.UserEmail = email
		res.UserSubject = sub
	}
	return res, nil
}

// RefreshAccessToken uses a stored refresh token to mint a new access token.
// Studio's upload helper calls this transparently before each upload window.
func (c *Client) RefreshAccessToken(ctx context.Context, refreshToken string) (string, time.Time, error) {
	if !c.Configured() {
		return "", time.Time{}, ErrOAuthNotConfigured
	}
	form := url.Values{
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	tok, err := c.postToken(ctx, form)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok.AccessToken, time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second), nil
}

// Revoke tells Google to invalidate the refresh token. Called on disconnect
// so the operator can re-grant cleanly later. Best-effort — we delete our
// row regardless of the result.
func (c *Client) Revoke(ctx context.Context, refreshToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/revoke?token="+url.QueryEscape(refreshToken), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("revoke %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// UserInfo calls Google's openid userinfo endpoint to grab email + subject.
// We hit this only on first connect (later refreshes use cached row).
func (c *Client) UserInfo(ctx context.Context, accessToken string) (email, subject string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://openidconnect.googleapis.com/v1/userinfo", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("userinfo %d: %s", resp.StatusCode, string(body))
	}
	var u struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return "", "", err
	}
	return u.Email, u.Sub, nil
}

// postToken POSTs the form-encoded body to /token and parses the JSON.
// Surfaces Google's `error` field as a real Go error so callers see
// "invalid_grant: Token has been expired or revoked." instead of "200 OK".
func (c *Client) postToken(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/token",
		bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token (%d): %s", resp.StatusCode, string(body))
	}
	if tok.Error != "" {
		return nil, fmt.Errorf("oauth %s: %s", tok.Error, tok.ErrorDesc)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("oauth http %d: %s", resp.StatusCode, string(body))
	}
	return &tok, nil
}

// ErrOAuthNotConfigured indicates the cloud server doesn't have a
// client_id/secret loaded. /operator/storage shows a "set GOOGLE_OAUTH_*"
// banner instead of a confusing OAuth 400 page.
var ErrOAuthNotConfigured = errors.New("drive: GOOGLE_OAUTH_CLIENT_ID / GOOGLE_OAUTH_CLIENT_SECRET not configured")

// ErrNoRefreshToken — Google didn't issue a refresh_token (most likely
// because we omitted prompt=consent on the redirect, or the operator already
// has an active grant). Operator is told to re-try; AuthorizeURL pins
// prompt=consent so a re-attempt always works.
var ErrNoRefreshToken = errors.New("drive: Google did not return a refresh_token (try disconnecting from your Google account first, then reconnect)")
