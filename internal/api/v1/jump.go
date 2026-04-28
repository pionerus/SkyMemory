package v1

import "time"

// SegmentKind enumerates the canonical 7 segments plus a generic custom prefix.
// Custom segments use the form "custom:<label>" — server treats anything starting
// with "custom:" as a generic kind without per-kind heuristics.
const (
	KindIntro          = "intro"
	KindInterviewPre   = "interview_pre"
	KindWalk           = "walk"
	KindInterviewPlane = "interview_plane"
	KindFreefall       = "freefall"
	KindLanding        = "landing"
	KindClosing        = "closing"
	KindCustomPrefix   = "custom:"
)

// JumpRegisterRequest creates the cloud-side jump record before any uploads happen.
type JumpRegisterRequest struct {
	// Client identity
	ClientName  string `json:"client_name"`
	ClientEmail string `json:"client_email,omitempty"`
	ClientPhone string `json:"client_phone,omitempty"`

	// Outputs chosen in Step 0 of the wizard
	Output1080p    bool `json:"output_1080p"`
	Output4K       bool `json:"output_4k"`
	OutputVertical bool `json:"output_vertical"`
	OutputPhotos   bool `json:"output_photos"`

	// True when operator is supplying their own DSLR photos (skip auto-screenshots)
	HasOperatorUploadedPhotos bool `json:"has_operator_uploaded_photos"`
}

type JumpRegisterResponse struct {
	JumpID     int64  `json:"jump_id"`
	ClientID   int64  `json:"client_id"`
	AccessCode string `json:"access_code"`
}

// ArtifactReport is one entry in JumpCompleteRequest — describes one uploaded file.
type ArtifactReport struct {
	Kind      string `json:"kind"`             // 'horizontal_1080p' | 'horizontal_4k' | 'vertical' | 'photo' | 'screenshot'
	Variant   string `json:"variant,omitempty"` // 'preview' | 'original' (only photos)
	S3Key     string `json:"s3_key"`
	ETag      string `json:"etag"`
	SizeBytes int64  `json:"size_bytes"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

type JumpCompleteRequest struct {
	// Regeneration=true means this is NOT the first time pipeline ran for this jump.
	// Cloud uses this to:
	//   1. Skip emitting `usage_events('video_generated')` (no double billing)
	//   2. Rotate `clients.access_code` so the old client link 404s
	Regeneration bool `json:"regeneration"`

	DurationSeconds int               `json:"duration_seconds"`
	MusicTrackID    int64             `json:"music_track_id,omitempty"`
	Artifacts       []ArtifactReport  `json:"artifacts"`
}

type JumpCompleteResponse struct {
	JumpID            int64  `json:"jump_id"`
	NewAccessCode     string `json:"new_access_code,omitempty"` // populated only on regeneration
	OldAccessCodeKept bool   `json:"old_access_code_kept,omitempty"`
}

// JumpSendRequest tells cloud to email the client. Operator-triggered, never automatic.
type JumpSendRequest struct {
	JumpID     int64  `json:"jump_id"`
	OverrideEmail string `json:"override_email,omitempty"` // optional, defaults to clients.email
}

type JumpSendResponse struct {
	SentAt time.Time `json:"sent_at"`
	To     string    `json:"to"`
}
