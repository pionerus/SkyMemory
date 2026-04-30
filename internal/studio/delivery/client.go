// Package delivery (studio side) ships rendered videos / photos up to the
// cloud after each successful render. Uses the two-step flow: ask cloud for
// a presigned PUT URL, upload to S3 directly (cloud doesn't proxy bytes),
// then POST a registration row so the watch page can find the file. Phase 7.1.
package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	v1 "github.com/pionerus/freefall/internal/api/v1"
)

// Client wraps cloud-facing HTTP calls. One instance per studio process.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		// Long timeout — uploads can be 50–500 MB. ffprobe + Stat have
		// already validated the file before we get here.
		hc: &http.Client{Timeout: 30 * time.Minute},
	}
}

// APIError mirrors cloud's {code, message}.
type APIError struct {
	HTTPStatus int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloud %d %s: %s", e.HTTPStatus, e.Code, e.Message)
}

// UploadAndRegister is the high-level orchestration: presign → PUT → register.
// kind is one of the jump_artifacts.kind enum values
// ('horizontal_1080p','horizontal_4k','vertical','photo','screenshot').
//
// Returns the registered artifact ID + the jump's resulting status — either
// can be discarded if the caller only cares about success/failure.
func (c *Client) UploadAndRegister(
	ctx context.Context,
	jumpID int64,
	kind string,
	localPath string,
	width, height int,
) (int64, string, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return 0, "", fmt.Errorf("stat %s: %w", localPath, err)
	}
	size := info.Size()
	if size <= 0 {
		return 0, "", fmt.Errorf("%s is empty", localPath)
	}

	// Step 1: ask cloud for a presigned PUT.
	presigned, err := c.requestUploadURL(ctx, jumpID, kind, size)
	if err != nil {
		return 0, "", fmt.Errorf("request upload URL: %w", err)
	}

	// Step 2: PUT to S3 directly.
	etag, err := c.putToS3(ctx, presigned, localPath, size)
	if err != nil {
		return 0, "", fmt.Errorf("upload: %w", err)
	}

	// Step 3: register the artifact row + bump jump status.
	resp, err := c.register(ctx, jumpID, v1.ArtifactRegisterRequest{
		Kind:      kind,
		S3Key:     presigned.S3Key,
		ETag:      etag,
		SizeBytes: size,
		Width:     width,
		Height:    height,
		Variant:   "original",
	})
	if err != nil {
		return 0, "", fmt.Errorf("register artifact: %w", err)
	}
	return resp.ArtifactID, resp.JumpStatus, nil
}

func (c *Client) requestUploadURL(ctx context.Context, jumpID int64, kind string, size int64) (*v1.ArtifactUploadURLResponse, error) {
	body, _ := json.Marshal(v1.ArtifactUploadURLRequest{Kind: kind, SizeBytes: size})
	url := c.baseURL + "/api/v1/jumps/" + strconv.FormatInt(jumpID, 10) + "/artifacts/upload-url"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	// Short-timeout sub-client for the JSON round-trips so a hung cloud
	// doesn't lock up the 30-min uploader.
	short := &http.Client{Timeout: 15 * time.Second}
	resp, err := short.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(respBody, resp.StatusCode)
	}
	var out v1.ArtifactUploadURLResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode upload-url: %w", err)
	}
	return &out, nil
}

// putToS3 streams the local file to the presigned URL. We do NOT use the
// `noFollow` redirect dance the music cache uses — presigned PUT signs the
// host header, so the URL is the final destination.
//
// Returns the S3 ETag header (sans surrounding quotes) for cloud to record.
func (c *Client) putToS3(ctx context.Context, presigned *v1.ArtifactUploadURLResponse, localPath string, size int64) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presigned.UploadURL, f)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	if presigned.ContentType != "" {
		req.Header.Set("Content-Type", presigned.ContentType)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("PUT to S3: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("S3 returned %d: %.500s", resp.StatusCode, errBody)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	return etag, nil
}

func (c *Client) register(ctx context.Context, jumpID int64, body v1.ArtifactRegisterRequest) (*v1.ArtifactRegisterResponse, error) {
	payload, _ := json.Marshal(body)
	url := c.baseURL + "/api/v1/jumps/" + strconv.FormatInt(jumpID, 10) + "/artifacts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	short := &http.Client{Timeout: 15 * time.Second}
	resp, err := short.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(respBody, resp.StatusCode)
	}
	var out v1.ArtifactRegisterResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode register: %w", err)
	}
	return &out, nil
}

func parseAPIError(body []byte, status int) error {
	var apiErr APIError
	if err := json.Unmarshal(bytes.TrimSpace(body), &apiErr); err == nil && apiErr.Code != "" {
		apiErr.HTTPStatus = status
		return &apiErr
	}
	return errors.New("cloud returned " + strconv.Itoa(status) + ": " + string(body))
}
