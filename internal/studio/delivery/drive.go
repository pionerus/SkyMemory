package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// driveFilename maps a kind+slot to a human-readable Drive filename.
func driveFilename(kind, slot string) string {
	switch kind {
	case "horizontal_1080p":
		return "edit_1080p.mp4"
	case "horizontal_4k":
		return "edit_4k.mp4"
	case "vertical":
		return "reel_vertical.mp4"
	case "wow_highlights":
		return "reel_wow.mp4"
	case "photo":
		if slot != "" {
			return "photo_" + slot + ".jpg"
		}
		return "photo.jpg"
	case "screenshot":
		if slot != "" {
			return "screenshot_" + slot + ".jpg"
		}
		return "screenshot.jpg"
	default:
		return kind + ".bin"
	}
}

// uploadToDrive streams a local file to the operator's Google Drive using
// the resumable upload protocol. Returns the Drive file id on success.
//
// The caller supplies an access token minted by the cloud's drive-token
// endpoint; this function never touches credentials directly.
func uploadToDrive(
	ctx context.Context,
	hc *http.Client,
	accessToken, folderID, kind, slot, contentType, localPath string,
	size int64,
) (string, error) {
	filename := driveFilename(kind, slot)

	// Step 1 — initiate resumable upload session.
	initBody, _ := json.Marshal(map[string]any{
		"name":    filename,
		"parents": []string{folderID},
	})
	initReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable&supportsAllDrives=true",
		bytes.NewReader(initBody),
	)
	if err != nil {
		return "", err
	}
	initReq.Header.Set("Authorization", "Bearer "+accessToken)
	initReq.Header.Set("Content-Type", "application/json")
	initReq.Header.Set("X-Upload-Content-Type", contentType)
	initReq.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))

	// Use a short-timeout client just for the initiation round-trip.
	initCtx, initCancel := context.WithTimeout(ctx, 20*time.Second)
	defer initCancel()
	initReq = initReq.WithContext(initCtx)

	initResp, err := hc.Do(initReq)
	if err != nil {
		return "", fmt.Errorf("drive resumable init: %w", err)
	}
	defer initResp.Body.Close()
	if initResp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(initResp.Body, 2048))
		return "", fmt.Errorf("drive resumable init %d: %s", initResp.StatusCode, errBody)
	}
	uploadURI := initResp.Header.Get("Location")
	if uploadURI == "" {
		return "", fmt.Errorf("drive resumable init: no Location header")
	}

	// Step 2 — stream the file to the resumable URI.
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURI, f)
	if err != nil {
		return "", err
	}
	putReq.ContentLength = size
	putReq.Header.Set("Content-Type", contentType)

	putResp, err := hc.Do(putReq)
	if err != nil {
		return "", fmt.Errorf("drive upload PUT: %w", err)
	}
	defer putResp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(putResp.Body, 8192))
	// 200 or 201 are both success for resumable uploads.
	if putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("drive upload PUT %d: %.500s", putResp.StatusCode, respBody)
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("drive upload: no file id in response: %s", string(respBody))
	}
	return out.ID, nil
}

// driveKey wraps a Drive file id into the storage-key convention understood
// by cloud's RegisterArtifact + watch page.
func driveKey(fileID string) string {
	return "drive:" + fileID
}

// isDriveKey reports whether an s3_key value refers to a Drive file.
func isDriveKey(key string) bool {
	return strings.HasPrefix(key, "drive:")
}
