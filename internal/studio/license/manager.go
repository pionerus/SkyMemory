package license

import (
	"context"
	"log"
	"sync"
	"time"
)

// Manager holds the latest validation Result and runs a background refresher.
// Read-locked Snapshot() is cheap enough that handlers can call it on every request.
type Manager struct {
	client   *Client
	token    string
	version  string
	interval time.Duration

	mu      sync.RWMutex
	current Result
	lastAt  time.Time // wall-clock time when `current` was last refreshed
}

// NewManager. interval is the refresh cadence; 0 means use 6h default.
func NewManager(client *Client, token, studioVersion string, interval time.Duration) *Manager {
	if interval == 0 {
		interval = 6 * time.Hour
	}
	return &Manager{client: client, token: token, version: studioVersion, interval: interval}
}

// Start runs an immediate validation, then refreshes on `interval` until ctx is canceled.
// Idempotent — call once at studio startup.
func (m *Manager) Start(ctx context.Context) {
	m.refresh(ctx)
	go func() {
		t := time.NewTicker(m.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.refresh(ctx)
			}
		}
	}()
}

func (m *Manager) refresh(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	res := m.client.Validate(rctx, m.token, m.version)

	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case res.IsTransientFailure():
		// Network blip — keep the last good result if we have one.
		log.Printf("license: transient failure (%v); keeping previous state", res.Err)
		return
	case !res.Valid:
		log.Printf("license: invalid (reason=%s)", res.Reason)
	default:
		log.Printf("license: ok — tenant=%q operator=%q valid_until=%s",
			res.TenantName, res.OperatorEmail, res.ValidUntil.Format(time.RFC3339))
	}
	m.current = res
	m.lastAt = time.Now()
}

// Snapshot returns the most recent Result. Cheap, lock-protected.
func (m *Manager) Snapshot() (Result, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current, m.lastAt
}
