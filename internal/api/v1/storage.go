package v1

import "time"

// Storage modes match tenant_storage_configs.mode CHECK constraint.
const (
	StorageModeS3          = "s3"
	StorageModeMinIO       = "minio"
	StorageModeCloudHosted = "cloud_hosted"
)

type StorageConfig struct {
	Mode         string `json:"mode"`
	EndpointURL  string `json:"endpoint_url,omitempty"`
	Region       string `json:"region,omitempty"`
	Bucket       string `json:"bucket"`
	AccessKeyID  string `json:"access_key_id"`
	SecretKey    string `json:"secret_key,omitempty"` // write-only — never returned by GET, mask shown instead
	UsePathStyle bool   `json:"use_path_style"`

	LastHealthCheckAt    *time.Time `json:"last_health_check_at,omitempty"`
	LastHealthCheckOK    bool       `json:"last_health_check_ok"`
	LastHealthCheckError string     `json:"last_health_check_error,omitempty"`
}

type StorageTestResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	StepFailed string `json:"step_failed,omitempty"` // 'put' | 'get' | 'delete'
}
