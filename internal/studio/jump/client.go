// Package jump owns the studio<->cloud calls for jump records — register,
// fetch status, complete (later). Auth is via the shared cookie-jar
// http.Client built in internal/studio/session — every request carries
// the operator's session cookie automatically.
package jump

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	v1 "github.com/pionerus/freefall/internal/api/v1"
)

// Client wraps HTTP calls to the cloud server. Construct once per studio process.
type Client struct {
	baseURL string
	hc      *http.Client
}

// NewClient takes the cookie-jar-backed http.Client from session.Manager.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: baseURL, hc: hc}
}

// APIError is returned when cloud responds with a non-2xx and a JSON {code, message}.
// Studio handlers can do `if errors.As(err, &apiErr) { ... }` to surface a friendly UI.
type APIError struct {
	HTTPStatus int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloud %d %s: %s", e.HTTPStatus, e.Code, e.Message)
}

// Register POSTs to /api/v1/jumps/register. Returns the new jump_id, client_id,
// and human-formatted access_code.
func (c *Client) Register(ctx context.Context, req v1.JumpRegisterRequest) (*v1.JumpRegisterResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/jumps/register", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var apiErr APIError
		if err := json.Unmarshal(respBytes, &apiErr); err == nil && apiErr.Code != "" {
			apiErr.HTTPStatus = resp.StatusCode
			return nil, &apiErr
		}
		return nil, fmt.Errorf("cloud returned %d: %s", resp.StatusCode, string(respBytes))
	}

	var out v1.JumpRegisterResponse
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if out.JumpID == 0 {
		return nil, errors.New("cloud returned empty jump_id")
	}
	return &out, nil
}
