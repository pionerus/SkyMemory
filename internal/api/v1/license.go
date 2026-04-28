package v1

import "time"

// LicenseValidateRequest is sent by studio at startup and once per 24h.
type LicenseValidateRequest struct {
	Token             string `json:"token"`
	DeviceFingerprint string `json:"device_fingerprint"` // hostname + MAC hash; for tracking, not auth
	StudioVersion     string `json:"studio_version"`
}

type LicenseValidateResponse struct {
	Valid       bool      `json:"valid"`
	TenantID    int64     `json:"tenant_id,omitempty"`
	OperatorID  int64     `json:"operator_id,omitempty"`
	TenantName  string    `json:"tenant_name,omitempty"`
	OperatorEmail string  `json:"operator_email,omitempty"`
	ValidUntil  time.Time `json:"valid_until,omitempty"` // server says "trust this answer until X"
	Reason      string    `json:"reason,omitempty"`      // populated when valid=false
}
