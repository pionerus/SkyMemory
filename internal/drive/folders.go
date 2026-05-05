package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// EnsureRootFolder returns a Drive folder id for the operator's "Skydive
// Memory" root. If `cachedID` is non-empty, we verify it via files.get
// (which works under drive.file scope because the folder was created BY
// this app). On 404 / trashed / empty cache we create a fresh folder.
//
// Why no find-by-name: drive.file scope can't list arbitrary folders by
// name+parent — files.list with `'root' in parents` returns 403
// "Insufficient Permission" (we'd need drive.metadata.readonly for that).
// Caching the id in our DB sidesteps the entire metadata-read scope.
func (c *Client) EnsureRootFolder(ctx context.Context, accessToken, cachedID string) (string, error) {
	const folderName = "Skydive Memory"
	if cachedID != "" {
		if ok, _ := c.fileExists(ctx, accessToken, cachedID); ok {
			return cachedID, nil
		}
	}
	return c.createFolder(ctx, accessToken, folderName, "root")
}

// EnsureJumpFolder is the per-jump variant. Same pattern: cached id wins,
// otherwise create a fresh folder under the supplied parent.
func (c *Client) EnsureJumpFolder(ctx context.Context, accessToken, parentID, folderName, cachedID string) (string, error) {
	if cachedID != "" {
		if ok, _ := c.fileExists(ctx, accessToken, cachedID); ok {
			return cachedID, nil
		}
	}
	return c.createFolder(ctx, accessToken, folderName, parentID)
}

// fileExists returns true when a file with the given id is reachable AND
// not trashed. 404 → false, no error. Other transport errors propagate so
// the caller can distinguish "doesn't exist" from "Drive is down".
func (c *Client) fileExists(ctx context.Context, accessToken, fileID string) (bool, error) {
	u := "https://www.googleapis.com/drive/v3/files/" + url.PathEscape(fileID) + "?fields=id,trashed"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("files.get %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		ID      string `json:"id"`
		Trashed bool   `json:"trashed"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return false, err
	}
	return out.ID != "" && !out.Trashed, nil
}

// MakePublic sets a file's permission to "anyone with the link can view"
// so the watch page's download button works without an extra auth dance.
// allowFileDiscovery=false keeps it out of search results (the only way to
// land on the file is to know the share URL).
func (c *Client) MakePublic(ctx context.Context, accessToken, fileID string) error {
	body, _ := json.Marshal(map[string]any{
		"role": "reader",
		"type": "anyone",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://www.googleapis.com/drive/v3/files/"+url.PathEscape(fileID)+"/permissions?supportsAllDrives=true",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("set public %d: %s", resp.StatusCode, string(errBody))
	}
	return nil
}

func (c *Client) createFolder(ctx context.Context, accessToken, name, parentID string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"name":     name,
		"mimeType": "application/vnd.google-apps.folder",
		"parents":  []string{parentID},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://www.googleapis.com/drive/v3/files?fields=id&supportsAllDrives=true",
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("create folder %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}
