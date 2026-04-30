package v1

// TenantBrandingResponse is what `GET /api/v1/tenant/branding` returns to studio.
// Each *Asset is nil-or-omitted when the tenant hasn't uploaded that slot.
//
// The `etag` field is the upstream S3 ETag (or whatever HEAD returns) — studio
// uses it as a cache-key suffix so re-uploads are detected without trusting
// path equality (the path is stable, content isn't).
type TenantBrandingResponse struct {
	TenantID  int64               `json:"tenant_id"`
	Watermark *WatermarkAsset     `json:"watermark,omitempty"`
	Intro     *BrandingClipAsset  `json:"intro,omitempty"`
	Outro     *BrandingClipAsset  `json:"outro,omitempty"`
}

// WatermarkAsset bundles the PNG download + the overlay parameters the
// studio pipeline needs to position it on the final video.
type WatermarkAsset struct {
	URL        string `json:"url"`           // 30-min presigned GET
	ETag       string `json:"etag"`          // cache-busting key
	SizePct    int    `json:"size_pct"`      // 5..25, % of output width
	OpacityPct int    `json:"opacity_pct"`   // 10..100
	Position   string `json:"position"`      // 'bottom-right' | 'bottom-left' | 'top-right' | 'top-left'
}

// BrandingClipAsset is the intro or outro mp4. Studio downloads, ffprobes
// once, and concats it on either side of the main timeline.
type BrandingClipAsset struct {
	URL  string `json:"url"`  // 30-min presigned GET
	ETag string `json:"etag"` // cache-busting key
}
