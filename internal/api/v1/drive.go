package v1

// DriveUploadTokenResponse is returned by GET /api/v1/jumps/{id}/drive-token.
// Studio calls this before uploading to learn whether the operator has Drive
// connected and, if so, receive the short-lived access token + per-jump folder.
type DriveUploadTokenResponse struct {
	Connected   bool   `json:"connected"`
	AccessToken string `json:"access_token,omitempty"`
	FolderID    string `json:"folder_id,omitempty"` // per-jump Drive folder id
}
