// Package music (studio side) wraps cloud-facing calls for the music library:
// catalog fetch, suggest, and "set music for jump" updates.
package music

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

// Client wraps HTTP calls to the cloud server. One instance per studio process.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		hc:      &http.Client{Timeout: 8 * time.Second},
	}
}

// APIError mirrors the cloud-side {code, message} response so studio handlers
// can surface a friendly message via errors.As.
type APIError struct {
	HTTPStatus int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloud %d %s: %s", e.HTTPStatus, e.Code, e.Message)
}

// Catalog returns the full catalog visible to the studio's tenant (global +
// tenant-owned). Each track carries a 15-min preview URL for inline playback.
func (c *Client) Catalog(ctx context.Context) (*v1.MusicListResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/music", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get music: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(body, resp.StatusCode)
	}
	var out v1.MusicListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode music list: %w", err)
	}
	return &out, nil
}

// SetJumpMusic posts the picked track id (or 0 to clear) up to cloud, scoped
// to the jump_id we got from /jumps/register.
func (c *Client) SetJumpMusic(ctx context.Context, jumpID, trackID int64) error {
	body, _ := json.Marshal(map[string]int64{"music_track_id": trackID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/api/v1/jumps/"+strconv.FormatInt(jumpID, 10)+"/music",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("put music: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return parseAPIError(respBody, resp.StatusCode)
	}
	return nil
}

func parseAPIError(body []byte, status int) error {
	var apiErr APIError
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Code != "" {
		apiErr.HTTPStatus = status
		return &apiErr
	}
	return errors.New("cloud returned " + strconv.Itoa(status) + ": " + string(body))
}
