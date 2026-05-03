package music

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// Cache is a tiny disk-backed cache of music tracks under
// ~/.freefall-studio/music-cache/<track_id>.mp3. Cache key is just track_id —
// catalog tracks are versionless (admin can't replace bytes for the same id;
// re-uploads create a new row). Multiple concurrent downloads of the same id
// are allowed but wasteful; we don't bother with a mutex because in practice
// a single render does one download per project.
type Cache struct {
	dir    string
	client *Client
}

func NewCache(dir string, client *Client) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache dir: %w", err)
	}
	return &Cache{dir: dir, client: client}, nil
}

// PathFor returns the canonical cache path for a track id, regardless of
// whether the file exists yet.
func (c *Cache) PathFor(trackID int64) string {
	return filepath.Join(c.dir, strconv.FormatInt(trackID, 10)+".mp3")
}

// Ensure returns the local path to the cached track, downloading it from
// cloud if missing. If the file already exists with non-zero size, it's
// returned without a network round-trip.
func (c *Cache) Ensure(ctx context.Context, trackID int64) (string, error) {
	if trackID <= 0 {
		return "", errors.New("trackID must be > 0")
	}
	dst := c.PathFor(trackID)
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return dst, nil
	}
	if err := c.client.Download(ctx, trackID, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// Download fetches the full track file via cloud's GET /api/v1/music/{id}/file
// (which 302-redirects to a 30-min presigned URL). Writes to dst path atomically
// (temp file + rename) so a half-fetched download doesn't poison the cache.
//
// We do the redirect MANUALLY — first GET to cloud (carries the session
// cookie via c.hc.Jar), expecting 302; second GET to the Location
// (different host, no cookie auto-attaches by jar scoping) — because
// S3-presigned URLs sign the host header and any extra Authorization
// would collide. The default Go redirect handler isn't strict enough
// about stripping cookies cross-host so we redirect by hand.
func (c *Client) Download(ctx context.Context, trackID int64, dst string) error {
	if trackID <= 0 {
		return errors.New("trackID must be > 0")
	}
	apiURL := c.baseURL + "/api/v1/music/" + strconv.FormatInt(trackID, 10) + "/file"

	// Step 1: ask cloud where the file is. Reuse the SAME cookie jar
	// as c.hc so the operator's session cookie attaches automatically.
	noFollow := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: c.hc.Timeout,
		Jar:     c.hc.Jar,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}

	resp, err := noFollow.Do(req)
	if err != nil {
		return fmt.Errorf("get track stub: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		if resp.StatusCode >= 400 {
			return parseAPIError(body, resp.StatusCode)
		}
		return fmt.Errorf("cloud returned %d (expected 302); body: %.200s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return errors.New("cloud 302 missing Location header")
	}

	// Step 2: download from S3/MinIO. Plain GET, no Authorization carryover —
	// the presigned URL is its own auth.
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return err
	}
	dlResp, err := c.hc.Do(dlReq)
	if err != nil {
		return fmt.Errorf("download from storage: %w", err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(dlResp.Body, 4096))
		return fmt.Errorf("storage returned %d: %.200s", dlResp.StatusCode, errBody)
	}

	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create cache file: %w", err)
	}
	if _, err := io.Copy(f, dlResp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write cache: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close cache: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename cache: %w", err)
	}
	return nil
}
