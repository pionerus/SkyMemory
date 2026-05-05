package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ServerConfig is read from env at boot. No defaults for production secrets.
type ServerConfig struct {
	Env             string // "development" | "production"
	HTTPAddr        string // ":8000"
	DatabaseURL     string // postgres://...
	RedisURL        string // redis://...
	SecretKey       string // session signing
	SentryDSN       string // optional, can also live in app_settings
	PublicBaseURL   string // e.g. https://app.freefall.ing — used for signed-link emails

	// S3-compatible object storage for the music library + (later) the
	// platform's "cloud-hosted" storage fallback for tenants that don't bring
	// their own bucket. Defaults match compose.yml's MinIO.
	MusicEndpoint     string // e.g. http://localhost:59000 (path-style for MinIO)
	MusicRegion       string // 'auto' / 'eu-central-1'
	MusicAccessKey    string
	MusicSecretKey    string
	MusicBucket       string // 'freefall-music'
	MusicUsePathStyle bool   // true for MinIO

	// Tenant branding (watermark PNG + optional intro/outro clips). Reuses
	// the same MinIO/S3 credentials as music — only the bucket differs so
	// branding stays a separate namespace from the global music library.
	BrandingBucket string // 'freefall-branding'

	// Final deliverables (rendered videos + photos) uploaded by studio.exe
	// after each render. Per-tenant prefix inside the bucket. Phase 7.1.
	DeliverablesBucket string // 'freefall-deliverables'

	// Google Drive OAuth — operator-delegated storage (Phase 9.4 / Drive
	// integration). Optional: when blank, /operator/storage shows a
	// "not configured" banner instead of a broken Connect button.
	GoogleOAuthClientID     string
	GoogleOAuthClientSecret string
	GoogleOAuthRedirectURL  string // built from PublicBaseURL when blank

	// SMTP relay for outbound email (Phase 13). For prod we point at
	// smtp.resend.com:587 with the platform Resend API key as the password;
	// for dev we point at MailHog (localhost:51025) so emails stay local.
	SMTPHost     string // smtp.resend.com / localhost
	SMTPPort     int    // 587 / 51025
	SMTPUsername string // "resend" for Resend; empty for MailHog
	SMTPPassword string // Resend API key; empty for MailHog
	SMTPFrom     string // "Skydive Memory <noreply@flowtark.com>"
}

// StudioConfig — minimal env for the local Windows binary. Studio
// authenticates against cloud with the operator's email + password
// (same credentials they use to sign into /operator/* in the browser);
// the legacy `STUDIO_LICENSE_TOKEN` field is preserved as a transition
// fallback so existing dev installs keep working until they migrate.
type StudioConfig struct {
	HTTPAddr     string // ":8080"
	StatePath    string // path to SQLite state.db
	CloudBaseURL string // points at the cloud server

	// New auth path: studio.exe POSTs these to /auth/login at boot,
	// then keeps the resulting session cookie for every /api/v1/* call.
	OperatorEmail    string
	OperatorPassword string

	// Legacy. Set to non-empty to fall back to the bearer-token API path.
	// Removed entirely once every dev install has migrated.
	LicenseToken string
}

func LoadServer() (*ServerConfig, error) {
	c := &ServerConfig{
		Env:           getenv("FREEFALL_ENV", "development"),
		HTTPAddr:      getenv("FREEFALL_HTTP_ADDR", ":8000"),
		DatabaseURL:   os.Getenv("FREEFALL_DATABASE_URL"),
		RedisURL:      os.Getenv("FREEFALL_REDIS_URL"),
		SecretKey:     os.Getenv("FREEFALL_SECRET_KEY"),
		SentryDSN:     os.Getenv("FREEFALL_SENTRY_DSN"),
		PublicBaseURL: getenv("FREEFALL_PUBLIC_BASE_URL", "http://localhost:8000"),

		// Music library storage. Defaults are dev MinIO from compose.yml.
		MusicEndpoint:     getenv("FREEFALL_MUSIC_ENDPOINT", "http://localhost:59000"),
		MusicRegion:       getenv("FREEFALL_MUSIC_REGION", "auto"),
		MusicAccessKey:    getenv("FREEFALL_MUSIC_ACCESS_KEY", "freefall"),
		MusicSecretKey:    getenv("FREEFALL_MUSIC_SECRET_KEY", "freefall_dev_secret"),
		MusicBucket:       getenv("FREEFALL_MUSIC_BUCKET", "freefall-music"),
		MusicUsePathStyle: getenv("FREEFALL_MUSIC_PATH_STYLE", "true") == "true",

		BrandingBucket: getenv("FREEFALL_BRANDING_BUCKET", "freefall-branding"),

		DeliverablesBucket: getenv("FREEFALL_DELIVERABLES_BUCKET", "freefall-deliverables"),

		GoogleOAuthClientID:     os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		GoogleOAuthClientSecret: os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		GoogleOAuthRedirectURL:  os.Getenv("GOOGLE_OAUTH_REDIRECT_URL"),

		SMTPHost:     getenv("SMTP_HOST", "localhost"),
		SMTPPort:     MustAtoi(os.Getenv("SMTP_PORT"), 51025),
		SMTPUsername: os.Getenv("SMTP_USERNAME"),
		SMTPPassword: os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:     getenv("SMTP_FROM", "Skydive Memory <noreply@flowtark.com>"),
	}
	// Default redirect URL — builds off the public base so dev/prod just
	// work without setting another env var.
	if c.GoogleOAuthRedirectURL == "" {
		c.GoogleOAuthRedirectURL = strings.TrimRight(c.PublicBaseURL, "/") + "/auth/google-drive/callback"
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("FREEFALL_DATABASE_URL is required")
	}
	if c.Env == "production" {
		if c.SecretKey == "" || strings.HasPrefix(c.SecretKey, "dev-") {
			return nil, fmt.Errorf("FREEFALL_SECRET_KEY must be set to a strong random value in production")
		}
	}
	if c.SecretKey == "" {
		c.SecretKey = "dev-secret-not-for-prod"
	}
	return c, nil
}

func LoadStudio() (*StudioConfig, error) {
	home, _ := os.UserHomeDir()
	return &StudioConfig{
		HTTPAddr:         getenv("STUDIO_HTTP_ADDR", "127.0.0.1:8080"),
		StatePath:        getenv("STUDIO_STATE_PATH", home+`\.freefall-studio\state.db`),
		CloudBaseURL:     getenv("STUDIO_CLOUD_BASE_URL", "http://localhost:8000"),
		OperatorEmail:    os.Getenv("STUDIO_OPERATOR_EMAIL"),
		OperatorPassword: os.Getenv("STUDIO_OPERATOR_PASSWORD"),
		LicenseToken:     os.Getenv("STUDIO_LICENSE_TOKEN"),
	}, nil
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// MustAtoi parses an int env var, falling back to def on error.
func MustAtoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
