// Package license owns the studio<->cloud license-validation channel.
//
// Studio calls Validate() at startup and every 6h thereafter. The result is held
// in memory only — on restart we re-validate. SQLite-backed offline grace will
// be added when we build "My Projects" persistence in a later iteration.
package license

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	v1 "github.com/pionerus/freefall/internal/api/v1"
)

// Client is a thin HTTP wrapper. Reuse one per studio process.
type Client struct {
	baseURL string
	hc      *http.Client
}

func NewClient(cloudBaseURL string) *Client {
	return &Client{
		baseURL: cloudBaseURL,
		hc: &http.Client{
			Timeout: 8 * time.Second,
		},
	}
}

// Result captures everything the UI / pipeline needs after a validation pass.
type Result struct {
	Valid         bool
	TenantID      int64
	OperatorID    int64
	TenantName    string
	OperatorEmail string
	ValidUntil    time.Time
	Reason        string // populated when !Valid
	Err           error  // network/decode error; distinct from a "valid:false" answer
}

// IsTransientFailure means we couldn't reach the cloud (network/timeout). Caller
// should keep using a previous valid Result if its ValidUntil hasn't passed.
func (r Result) IsTransientFailure() bool {
	return r.Err != nil
}

// Validate POSTs to /api/v1/license/validate. Caller supplies the token (typically
// from STUDIO_LICENSE_TOKEN env). studioVersion is reported for diagnostics only.
func (c *Client) Validate(ctx context.Context, token, studioVersion string) Result {
	if token == "" {
		return Result{Valid: false, Reason: "token_missing"}
	}

	req := v1.LicenseValidateRequest{
		Token:             token,
		DeviceFingerprint: deviceFingerprint(),
		StudioVersion:     studioVersion,
	}
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/license/validate", bytes.NewReader(body))
	if err != nil {
		return Result{Err: fmt.Errorf("build request: %w", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return Result{Err: fmt.Errorf("post: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return Result{Err: fmt.Errorf("cloud returned %d", resp.StatusCode)}
	}

	var out v1.LicenseValidateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{Err: fmt.Errorf("decode: %w", err)}
	}

	return Result{
		Valid:         out.Valid,
		TenantID:      out.TenantID,
		OperatorID:    out.OperatorID,
		TenantName:    out.TenantName,
		OperatorEmail: out.OperatorEmail,
		ValidUntil:    out.ValidUntil,
		Reason:        out.Reason,
	}
}

// deviceFingerprint returns a stable-ish identifier for the operator's machine.
// Hostname is used because it's stable across reboots and doesn't require any
// platform-specific code. Will be hashed before sending to cloud later.
func deviceFingerprint() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}
