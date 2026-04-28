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
}

// StudioConfig — minimal env for the local Windows binary.
type StudioConfig struct {
	HTTPAddr      string // ":8080"
	StatePath     string // path to SQLite state.db
	CloudBaseURL  string // points at the cloud server
	LicenseToken  string // provisioned via /admin in cloud
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
		HTTPAddr:     getenv("STUDIO_HTTP_ADDR", "127.0.0.1:8080"),
		StatePath:    getenv("STUDIO_STATE_PATH", home+`\.freefall-studio\state.db`),
		CloudBaseURL: getenv("STUDIO_CLOUD_BASE_URL", "http://localhost:8000"),
		LicenseToken: os.Getenv("STUDIO_LICENSE_TOKEN"),
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
