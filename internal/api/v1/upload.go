package v1

// UploadInitRequest asks the cloud server to mint presigned PUT URLs against
// the tenant's configured S3-compatible storage. Studio includes one entry per
// artifact it intends to upload.
type UploadInitRequest struct {
	JumpID    int64               `json:"jump_id"`
	Artifacts []UploadInitArtifact `json:"artifacts"`
}

type UploadInitArtifact struct {
	Kind    string `json:"kind"`              // 'horizontal_1080p' | 'horizontal_4k' | 'vertical' | 'photo' | 'screenshot'
	Variant string `json:"variant,omitempty"` // 'preview' | 'original'
	// SizeBytes lets server pick single-PUT vs multipart. >2 GB → multipart required.
	SizeBytes int64 `json:"size_bytes"`
	// PhotoIndex disambiguates multiple photos in one jump (0..N). Ignored for video kinds.
	PhotoIndex int `json:"photo_index,omitempty"`
}

type UploadInitResponse struct {
	JumpID    int64                `json:"jump_id"`
	Artifacts []UploadInitTarget   `json:"artifacts"`
}

type UploadInitTarget struct {
	Kind        string `json:"kind"`
	Variant     string `json:"variant,omitempty"`
	PhotoIndex  int    `json:"photo_index,omitempty"`
	S3Key       string `json:"s3_key"`        // canonical key the server expects in /complete
	UploadID    string `json:"upload_id,omitempty"`     // multipart upload id (empty for single PUT)
	PresignedPUT string `json:"presigned_put,omitempty"` // single-PUT URL (when SizeBytes ≤ 2 GB)
	PartURLs    []string `json:"part_urls,omitempty"`   // multipart URLs (when SizeBytes > 2 GB)
	PartSize    int64  `json:"part_size,omitempty"`     // size each part (last part may be smaller)
	ExpiresAt   string `json:"expires_at"`              // RFC 3339
}
