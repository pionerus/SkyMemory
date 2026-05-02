// Package session is studio.exe's auth client. It logs the operator into
// the cloud server with their email + password (the same credentials they
// use to sign into /operator/*) and keeps the resulting session cookie in a
// cookie jar so every subsequent /api/v1/* call goes through automatically.
//
// Replaces the old internal/studio/license package, which talked to the
// cloud over a one-shot bearer token issued by /admin/tokens. The token
// model required club admins to provision a separate "machine credential"
// per studio.exe install; the new model just uses the operator's normal
// login. License-token plumbing stays in the cloud for backwards-compat
// with older studio binaries but is no longer the primary path.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"time"
)

// Manager is the studio's single source of truth for "are we logged in".
// Construct one at boot, share its `Client()` with every cloud-talking
// package (music, branding, delivery, jump). The cookie jar makes it
// transparent: each Do() carries the session cookie automatically.
type Manager struct {
	baseURL  string
	email    string
	password string
	hc       *http.Client

	mu       sync.RWMutex
	current  Snapshot
	loggedAt time.Time
}

// Snapshot is the latest known state of the session. Reads via Snapshot()
// are cheap — handlers can call them on every request to populate the
// "License OK / Cloud connected" badge in the UI.
type Snapshot struct {
	Valid         bool
	OperatorID    int64
	TenantID      int64
	OperatorEmail string
	OperatorRole  string
	TenantSlug    string
	Reason        string // populated when Valid=false
	Err           error  // network / decode error; distinct from a clean "no" answer
}

// IsTransientFailure: caller can keep using a previous valid Snapshot if
// its `loggedAt` is still recent.
func (s Snapshot) IsTransientFailure() bool { return s.Err != nil }

// NewManager. Caller passes the shared http.Client AFTER constructing it
// via NewClient() so cookie state stays attached.
func NewManager(baseURL, email, password string) *Manager {
	jar, _ := cookiejar.New(nil)
	return &Manager{
		baseURL:  baseURL,
		email:    email,
		password: password,
		hc: &http.Client{
			Timeout: 10 * time.Second,
			Jar:     jar,
		},
	}
}

// Client returns the cookie-jar-backed http.Client. Pass this to every
// downstream cloud client (music.NewClient(baseURL, mgr.Client()), …).
// All requests made through it share the same session cookie.
func (m *Manager) Client() *http.Client { return m.hc }

// Login posts the operator's credentials to /auth/login and stores the
// resulting session cookie inside the manager's cookie jar. Returns the
// updated snapshot. Idempotent — call on boot, on demand, and on the
// first 401 from a downstream call.
func (m *Manager) Login(ctx context.Context) (Snapshot, error) {
	if m.email == "" || m.password == "" {
		snap := Snapshot{Valid: false, Reason: "credentials_missing"}
		m.set(snap)
		return snap, errors.New("STUDIO_OPERATOR_EMAIL / STUDIO_OPERATOR_PASSWORD not configured")
	}
	body, _ := json.Marshal(map[string]string{
		"email":    m.email,
		"password": m.password,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/auth/login", bytes.NewReader(body))
	if err != nil {
		snap := Snapshot{Err: err}
		m.set(snap)
		return snap, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.hc.Do(req)
	if err != nil {
		snap := Snapshot{Err: fmt.Errorf("post /auth/login: %w", err)}
		m.set(snap)
		return snap, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(respBody, &apiErr)
		reason := apiErr.Code
		if reason == "" {
			reason = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		snap := Snapshot{Valid: false, Reason: reason}
		m.set(snap)
		return snap, fmt.Errorf("login failed (%s): %s", reason, apiErr.Message)
	}

	// /auth/login response shape (see cmd/server's authH.Login):
	//   { operator_id, tenant_id, tenant_slug, email, role }
	var ok struct {
		OperatorID int64  `json:"operator_id"`
		TenantID   int64  `json:"tenant_id"`
		TenantSlug string `json:"tenant_slug"`
		Email      string `json:"email"`
		Role       string `json:"role"`
	}
	if err := json.Unmarshal(respBody, &ok); err != nil {
		snap := Snapshot{Err: fmt.Errorf("decode login: %w", err)}
		m.set(snap)
		return snap, err
	}
	snap := Snapshot{
		Valid:         true,
		OperatorID:    ok.OperatorID,
		TenantID:      ok.TenantID,
		TenantSlug:    ok.TenantSlug,
		OperatorEmail: ok.Email,
		OperatorRole:  ok.Role,
	}
	m.set(snap)
	return snap, nil
}

// EnsureLogin re-authenticates if the snapshot says we're not currently
// valid. Used by downstream clients before every API call so a session
// cookie that expired (or was revoked) gets refreshed transparently.
func (m *Manager) EnsureLogin(ctx context.Context) (Snapshot, error) {
	if s, _ := m.SnapshotState(); s.Valid {
		return s, nil
	}
	return m.Login(ctx)
}

// Start runs an initial Login + a background re-login on `interval`
// (default 6h, 0 = use default). Idempotent.
func (m *Manager) Start(ctx context.Context, interval time.Duration) {
	if interval == 0 {
		interval = 6 * time.Hour
	}
	if _, err := m.Login(ctx); err != nil {
		log.Printf("session login: %v", err)
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := m.Login(ctx); err != nil {
					log.Printf("session refresh: %v", err)
				}
			}
		}
	}()
}

// SnapshotState returns (snap, lastLoggedAt). Cheap; lock-protected.
func (m *Manager) SnapshotState() (Snapshot, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current, m.loggedAt
}

func (m *Manager) set(s Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = s
	if s.Valid {
		m.loggedAt = time.Now()
		log.Printf("session: ok — operator=%q tenant=%q role=%s",
			s.OperatorEmail, s.TenantSlug, s.OperatorRole)
	} else if s.Reason != "" {
		log.Printf("session: invalid (reason=%s)", s.Reason)
	}
}

// reserve url import in case future refresh endpoints get wired
var _ = url.Parse
