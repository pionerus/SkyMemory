// Package branding (studio side) fetches the tenant's branding bundle from
// cloud and caches the binary assets (watermark PNG, intro/outro mp4) on
// disk so the pipeline can reuse them across renders without a network round
// trip per generation.
//
// Cache layout (parent dir is constructor-supplied, typically
// ~/.freefall-studio/branding-cache):
//
//	<tenant_id>/manifest.json    JSON of cached etags + watermark settings
//	<tenant_id>/watermark.png    binary, downloaded on first miss
//	<tenant_id>/intro.mp4        binary, downloaded on first miss
//	<tenant_id>/outro.mp4        binary, downloaded on first miss
//
// Re-downloads happen when the cloud-reported ETag differs from the manifest's
// recorded value. Operators replacing a watermark in the admin UI propagate
// to studio on the next render.
package branding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	v1 "github.com/pionerus/freefall/internal/api/v1"
)

// Client wraps the cloud-facing HTTP calls. Auth is via the cookie jar
// inside hc — every request carries the operator's session cookie.
type Client struct {
	baseURL string
	hc      *http.Client
}

// NewClient takes the cookie-jar-backed *http.Client from session.Manager.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: baseURL, hc: hc}
}

// APIError mirrors the cloud-side {code, message} response.
type APIError struct {
	HTTPStatus int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloud %d %s: %s", e.HTTPStatus, e.Code, e.Message)
}

// Fetch returns the tenant's branding bundle: presigned URLs + ETags +
// watermark overlay parameters. Slots the operator hasn't uploaded come back
// nil; the response itself is never nil on success.
func (c *Client) Fetch(ctx context.Context) (*v1.TenantBrandingResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/tenant/branding", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get branding: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(body, resp.StatusCode)
	}
	var out v1.TenantBrandingResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode branding: %w", err)
	}
	return &out, nil
}

// downloadAsset GETs a presigned URL into dst atomically. Presigned URLs
// authenticate via the URL signature itself; we explicitly skip following
// redirects with auth carryover (matches the music-cache rationale: S3
// rejects requests where Authorization collides with the signed URL).
func downloadAsset(ctx context.Context, hc *http.Client, presignedURL, dst string) error {
	if presignedURL == "" {
		return errors.New("presignedURL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, presignedURL, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("storage returned %d: %.200s", resp.StatusCode, body)
	}
	return writeAtomic(dst, resp.Body)
}

func parseAPIError(body []byte, status int) error {
	var apiErr APIError
	if err := json.Unmarshal(bytes.TrimSpace(body), &apiErr); err == nil && apiErr.Code != "" {
		apiErr.HTTPStatus = status
		return &apiErr
	}
	return errors.New("cloud returned " + strconv.Itoa(status) + ": " + string(body))
}
