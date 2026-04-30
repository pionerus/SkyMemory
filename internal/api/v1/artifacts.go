package v1

// Phase 7.1 — studio → cloud delivery upload contract.
//
// Two-step flow:
//   1. POST /api/v1/jumps/{id}/artifacts/upload-url with kind+size_bytes
//      returns { upload_url, s3_key, expires_in_sec }.
//   2. Studio uploads the file directly to S3 via HTTP PUT to upload_url
//      (Content-Type must match what was signed).
//   3. POST /api/v1/jumps/{id}/artifacts with { kind, s3_key, size_bytes,
//      etag, width, height } inserts a jump_artifacts row and bumps
//      jump.status to 'ready'.
//
// We split the upload from the registration so the cloud server doesn't
// have to proxy 50–500 MB through itself.

// ArtifactUploadURLRequest asks cloud for a presigned PUT URL.
type ArtifactUploadURLRequest struct {
	Kind      string `json:"kind"`       // 'horizontal_1080p' | 'horizontal_4k' | 'vertical' | 'photo' | 'screenshot'
	SizeBytes int64  `json:"size_bytes"` // for sanity validation only — S3 doesn't enforce
}

// ArtifactUploadURLResponse carries the presigned PUT URL + the S3 key the
// studio must echo back when registering the artifact.
type ArtifactUploadURLResponse struct {
	UploadURL    string `json:"upload_url"`
	S3Key        string `json:"s3_key"`
	ContentType  string `json:"content_type"`
	ExpiresInSec int    `json:"expires_in_sec"`
}

// ArtifactRegisterRequest is sent after the upload completes. ETag and
// SizeBytes are echoed back from the S3 PUT response so cloud can record
// the canonical content version + size without a HEAD round-trip.
type ArtifactRegisterRequest struct {
	Kind      string `json:"kind"`
	S3Key     string `json:"s3_key"`
	ETag      string `json:"etag"`
	SizeBytes int64  `json:"size_bytes"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	// Variant defaults to "original" when empty. Phase 7 photos will use
	// "preview" for watermarked previews and "original" for paid downloads.
	Variant string `json:"variant,omitempty"`
}

// ArtifactRegisterResponse echoes the new row id + the jump's resulting
// status (ready / sent / etc).
type ArtifactRegisterResponse struct {
	ArtifactID int64  `json:"artifact_id"`
	JumpStatus string `json:"jump_status"`
}
