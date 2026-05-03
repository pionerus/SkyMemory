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

// AssignedClient mirrors a row from /api/v1/operator/clients — clients that
// the club admin has assigned to this operator. Drives the "pick client"
// dropdown on the studio's new-project flow. Status follows the canonical
// 5-step lifecycle: new → assigned → in_progress → sent → downloaded.
type AssignedClient struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Email        string    `json:"email,omitempty"`
	Phone        string    `json:"phone,omitempty"`
	AccessCode   string    `json:"access_code"`
	LatestJumpAt time.Time `json:"latest_jump_at,omitempty"`
	Status       string    `json:"status"`
	JumpCount    int       `json:"jump_count"`
}

// AssignedClients fetches /api/v1/operator/clients. On 401 the caller should
// trigger a session refresh and retry — handled at the session layer in main.
func (c *Client) AssignedClients(ctx context.Context) ([]AssignedClient, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/operator/clients", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cloud returned %d: %s", resp.StatusCode, string(respBytes))
	}
	var out struct {
		Clients []AssignedClient `json:"clients"`
	}
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out.Clients, nil
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
