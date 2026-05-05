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
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	v1 "github.com/pionerus/freefall/internal/api/v1"
)

// Client wraps cloud-facing HTTP calls. Auth is via the cookie jar
// inside the supplied http.Client (built in session.Manager).
type Client struct {
	baseURL string
	hc      *http.Client
}

// NewClient takes a cookie-jar-backed *http.Client. The hc must have a
// long timeout — uploads can be 50-500 MB.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Minute}
	}
	return &Client{baseURL: baseURL, hc: hc}
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
	return c.UploadAndRegisterWithSlot(ctx, jumpID, kind, "", localPath, width, height)
}

// UploadAndRegisterWithSlot is the multi-instance variant. Slot disambiguates
// the S3 key for kinds where many uploads land on the same jump (photo,
// screenshot). Single-instance kinds (1080p, 4k, vertical, wow_highlights)
// ignore the slot — pass "" or use UploadAndRegister.
//
// If the operator has Google Drive connected, the file is uploaded there and
// s3_key is stored as "drive:<fileId>". Falls back to S3/MinIO otherwise or
// on Drive upload error.
func (c *Client) UploadAndRegisterWithSlot(
	ctx context.Context,
	jumpID int64,
	kind string,
	slot string,
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

	// Try Google Drive first.
	if tok, derr := c.getDriveToken(ctx, jumpID); derr == nil && tok.Connected {
		contentType := contentTypeForKind(kind)
		fileID, uerr := uploadToDrive(ctx, c.hc, tok.AccessToken, tok.FolderID, kind, slot, contentType, localPath, size)
		if uerr != nil {
			log.Printf("drive upload failed, falling back to S3: %v", uerr)
		} else {
			resp, rerr := c.register(ctx, jumpID, v1.ArtifactRegisterRequest{
				Kind:      kind,
				S3Key:     driveKey(fileID),
				SizeBytes: size,
				Width:     width,
				Height:    height,
				Variant:   "original",
			})
			if rerr != nil {
				return 0, "", fmt.Errorf("register drive artifact: %w", rerr)
			}
			return resp.ArtifactID, resp.JumpStatus, nil
		}
	}

	// S3 path (Drive not connected or upload failed).
	presigned, err := c.requestUploadURL(ctx, jumpID, kind, slot, size)
	if err != nil {
		return 0, "", fmt.Errorf("request upload URL: %w", err)
	}
	etag, err := c.putToS3(ctx, presigned, localPath, size)
	if err != nil {
		return 0, "", fmt.Errorf("upload: %w", err)
	}
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

// getDriveToken asks the cloud whether this operator has Drive connected.
// Returns {Connected:false} (no error) when Drive is not set up.
func (c *Client) getDriveToken(ctx context.Context, jumpID int64) (*v1.DriveUploadTokenResponse, error) {
	url := c.baseURL + "/api/v1/jumps/" + strconv.FormatInt(jumpID, 10) + "/drive-token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	subCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req = req.WithContext(subCtx)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("drive-token %d: %s", resp.StatusCode, string(body))
	}
	var out v1.DriveUploadTokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode drive-token: %w", err)
	}
	return &out, nil
}

// SendDeliverablesEmailResp mirrors the cloud response shape so callers can
// log "already_sent" without parsing JSON twice.
type SendDeliverablesEmailResp struct {
	Sent      bool   `json:"sent"`
	Reason    string `json:"reason"`
	Recipient string `json:"recipient,omitempty"`
}

// SendDeliverablesEmail asks the cloud to email the jumper the watch link.
// Best-effort — studio logs the result but doesn't fail the render on a
// transient SMTP error (the watch page works regardless).
func (c *Client) SendDeliverablesEmail(ctx context.Context, jumpID int64, force bool) (*SendDeliverablesEmailResp, error) {
	body, _ := json.Marshal(map[string]any{"force": force})
	url := c.baseURL + "/api/v1/jumps/" + strconv.FormatInt(jumpID, 10) + "/send-email"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	subCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	req = req.WithContext(subCtx)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(respBody, resp.StatusCode)
	}
	var out SendDeliverablesEmailResp
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode send-email: %w", err)
	}
	return &out, nil
}

// contentTypeForKind returns the MIME type used for a Drive upload.
func contentTypeForKind(kind string) string {
	switch kind {
	case "photo", "screenshot":
		return "image/jpeg"
	default:
		return "video/mp4"
	}
}

func (c *Client) requestUploadURL(ctx context.Context, jumpID int64, kind, slot string, size int64) (*v1.ArtifactUploadURLResponse, error) {
	body, _ := json.Marshal(v1.ArtifactUploadURLRequest{Kind: kind, SizeBytes: size, Slot: slot})
	url := c.baseURL + "/api/v1/jumps/" + strconv.FormatInt(jumpID, 10) + "/artifacts/upload-url"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Use the shared cookie-jar client so the operator session cookie ships
	// with the request. Per-call timeout via the supplied ctx — caller
	// passes a 35-min deadline for the full upload chain; this single JSON
	// round-trip won't sit on it.
	subCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req = req.WithContext(subCtx)
	resp, err := c.hc.Do(req)
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
	req.Header.Set("Content-Type", "application/json")

	// Same cookie-jar client used for upload-url; per-call timeout via ctx.
	subCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req = req.WithContext(subCtx)
	resp, err := c.hc.Do(req)
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
