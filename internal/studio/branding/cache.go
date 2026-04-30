package branding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Bundle is what the pipeline consumes — local file paths plus the watermark
// overlay parameters. Empty path strings mean "this slot is not configured".
type Bundle struct {
	WatermarkPath       string
	WatermarkSizePct    int
	WatermarkOpacityPct int
	WatermarkPosition   string

	IntroPath string
	OutroPath string
}

// HasWatermark / HasIntro / HasOutro shorten checks at the call site.
func (b Bundle) HasWatermark() bool { return b.WatermarkPath != "" }
func (b Bundle) HasIntro() bool     { return b.IntroPath != "" }
func (b Bundle) HasOutro() bool     { return b.OutroPath != "" }

// Cache fetches the cloud-published branding for the studio's tenant and
// keeps local copies on disk. Re-downloads only when the upstream ETag
// changes; safe to call on every Generate.
type Cache struct {
	dir    string
	client *Client
}

func NewCache(dir string, client *Client) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir branding cache: %w", err)
	}
	return &Cache{dir: dir, client: client}, nil
}

// manifest is the on-disk record of "what does the cache currently hold for
// this tenant". Stored next to the binary blobs so a manual `rm -rf` of the
// cache dir is sufficient to force a full re-download next render.
type manifest struct {
	WatermarkETag       string `json:"watermark_etag,omitempty"`
	WatermarkSizePct    int    `json:"watermark_size_pct,omitempty"`
	WatermarkOpacityPct int    `json:"watermark_opacity_pct,omitempty"`
	WatermarkPosition   string `json:"watermark_position,omitempty"`

	IntroETag string `json:"intro_etag,omitempty"`
	OutroETag string `json:"outro_etag,omitempty"`
}

// Ensure fetches the cloud bundle and returns local paths to all slots that
// the operator has uploaded. Re-downloads only when ETag has changed since
// the last Ensure call. tenantID scopes the cache so a studio used across
// tenants (developer setup) doesn't conflate them.
func (c *Cache) Ensure(ctx context.Context, tenantID int64) (Bundle, error) {
	if tenantID <= 0 {
		return Bundle{}, errors.New("tenantID must be > 0")
	}
	tenantDir := filepath.Join(c.dir, strconv.FormatInt(tenantID, 10))
	if err := os.MkdirAll(tenantDir, 0o755); err != nil {
		return Bundle{}, fmt.Errorf("mkdir tenant cache: %w", err)
	}

	resp, err := c.client.Fetch(ctx)
	if err != nil {
		return Bundle{}, err
	}

	prev := loadManifest(tenantDir)
	cur := manifest{}
	bundle := Bundle{}

	if resp.Watermark != nil {
		dst := filepath.Join(tenantDir, "watermark.png")
		if err := ensureAsset(ctx, c.client.hc, resp.Watermark.URL, resp.Watermark.ETag, prev.WatermarkETag, dst); err != nil {
			return Bundle{}, fmt.Errorf("watermark: %w", err)
		}
		cur.WatermarkETag = resp.Watermark.ETag
		cur.WatermarkSizePct = resp.Watermark.SizePct
		cur.WatermarkOpacityPct = resp.Watermark.OpacityPct
		cur.WatermarkPosition = resp.Watermark.Position
		bundle.WatermarkPath = dst
		bundle.WatermarkSizePct = resp.Watermark.SizePct
		bundle.WatermarkOpacityPct = resp.Watermark.OpacityPct
		bundle.WatermarkPosition = resp.Watermark.Position
	} else {
		// Slot was removed cloud-side; clean up any stale cached blob so a
		// future re-upload doesn't accidentally serve an old asset.
		_ = os.Remove(filepath.Join(tenantDir, "watermark.png"))
	}

	if resp.Intro != nil {
		dst := filepath.Join(tenantDir, "intro.mp4")
		if err := ensureAsset(ctx, c.client.hc, resp.Intro.URL, resp.Intro.ETag, prev.IntroETag, dst); err != nil {
			return Bundle{}, fmt.Errorf("intro: %w", err)
		}
		cur.IntroETag = resp.Intro.ETag
		bundle.IntroPath = dst
	} else {
		_ = os.Remove(filepath.Join(tenantDir, "intro.mp4"))
	}

	if resp.Outro != nil {
		dst := filepath.Join(tenantDir, "outro.mp4")
		if err := ensureAsset(ctx, c.client.hc, resp.Outro.URL, resp.Outro.ETag, prev.OutroETag, dst); err != nil {
			return Bundle{}, fmt.Errorf("outro: %w", err)
		}
		cur.OutroETag = resp.Outro.ETag
		bundle.OutroPath = dst
	} else {
		_ = os.Remove(filepath.Join(tenantDir, "outro.mp4"))
	}

	if err := saveManifest(tenantDir, cur); err != nil {
		// Manifest write failure isn't fatal — next call will redo the diff.
		// Log silently by ignoring; pipeline can still consume the bundle.
		_ = err
	}
	return bundle, nil
}

// ensureAsset downloads the presigned URL when the cached file is missing or
// when the upstream ETag differs from what the manifest recorded last time.
//
// An empty cloud-side ETag (S3 backend that doesn't expose one) forces a
// download every time; that's correct behaviour — we lose the cache benefit
// but we never serve stale bytes.
func ensureAsset(ctx context.Context, hc *http.Client, presignedURL, curETag, prevETag, dst string) error {
	info, statErr := os.Stat(dst)
	stale := statErr != nil ||
		info.Size() == 0 ||
		curETag == "" ||
		curETag != prevETag
	if !stale {
		return nil
	}
	return downloadAsset(ctx, hc, presignedURL, dst)
}

func loadManifest(tenantDir string) manifest {
	var m manifest
	b, err := os.ReadFile(filepath.Join(tenantDir, "manifest.json"))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func saveManifest(tenantDir string, m manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicBytes(filepath.Join(tenantDir, "manifest.json"), b)
}

// writeAtomic streams body into <dst>.part then renames into place. Used for
// binary blobs (PNG/MP4); buffer-free.
func writeAtomic(dst string, body io.Reader) error {
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("copy body: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// writeAtomicBytes is the small-payload variant for the manifest file.
func writeAtomicBytes(dst string, payload []byte) error {
	tmp := dst + ".part"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// reserve `time` import: kept here for future TTL-based eviction; silence
// the linter in the meantime.
var _ = time.Hour
