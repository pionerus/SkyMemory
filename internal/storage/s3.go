// Package storage wraps the AWS SDK v2 S3 client so the rest of the codebase
// can talk to whichever S3-compatible service is configured (Hetzner Object
// Storage, Backblaze B2, MinIO on the operator's NAS, AWS S3, …).
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/pionerus/freefall/internal/config"
)

// Client wraps an *s3.Client + presigner. One instance per (endpoint, bucket).
type Client struct {
	S3       *s3.Client
	Presign  *s3.PresignClient
	Bucket   string
	Endpoint string
}

// NewMusicClient builds the client used for the music library, reading
// FREEFALL_MUSIC_* from cfg.
func NewMusicClient(cfg *config.ServerConfig) (*Client, error) {
	return newClient(cfg.MusicEndpoint, cfg.MusicRegion, cfg.MusicAccessKey,
		cfg.MusicSecretKey, cfg.MusicBucket, cfg.MusicUsePathStyle)
}

// NewBrandingClient reuses the same S3-compatible endpoint as the music
// library — same MinIO instance in dev — but writes to a separate bucket
// so the namespaces don't collide.
func NewBrandingClient(cfg *config.ServerConfig) (*Client, error) {
	return newClient(cfg.MusicEndpoint, cfg.MusicRegion, cfg.MusicAccessKey,
		cfg.MusicSecretKey, cfg.BrandingBucket, cfg.MusicUsePathStyle)
}

// NewDeliverablesClient holds rendered videos and photos uploaded by studio
// after every successful render. Same MinIO/S3 credentials, separate bucket
// — per-tenant prefixes inside the bucket keep namespaces clean.
func NewDeliverablesClient(cfg *config.ServerConfig) (*Client, error) {
	return newClient(cfg.MusicEndpoint, cfg.MusicRegion, cfg.MusicAccessKey,
		cfg.MusicSecretKey, cfg.DeliverablesBucket, cfg.MusicUsePathStyle)
}

func newClient(endpoint, region, ak, sk, bucket string, usePathStyle bool) (*Client, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket is empty")
	}
	if region == "" {
		region = "auto"
	}
	awsCfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(ak, sk, ""),
	}

	opts := []func(*s3.Options){
		func(o *s3.Options) {
			if endpoint != "" {
				o.BaseEndpoint = aws.String(endpoint)
			}
			o.UsePathStyle = usePathStyle
			o.Region = region
		},
	}
	cli := s3.NewFromConfig(awsCfg, opts...)
	return &Client{
		S3:       cli,
		Presign:  s3.NewPresignClient(cli),
		Bucket:   bucket,
		Endpoint: endpoint,
	}, nil
}

// EnsureBucket creates the bucket if it doesn't exist. Idempotent — called once
// at boot. AlreadyOwnedByYou / AlreadyExists are treated as success.
func (c *Client) EnsureBucket(ctx context.Context) error {
	_, err := c.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(c.Bucket)})
	if err == nil {
		return nil
	}
	// Try to create — if the error wasn't simply "not found", bubble it.
	_, cerr := c.S3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(c.Bucket)})
	if cerr == nil {
		return nil
	}
	if isAlreadyOwned(cerr) {
		return nil
	}
	return fmt.Errorf("ensure bucket %q: head=%v create=%v", c.Bucket, err, cerr)
}

// PutObject uploads body to the bucket under key. Caller controls Content-Type
// (e.g. "audio/mpeg" for music tracks).
func (c *Client) PutObject(ctx context.Context, key, contentType string, body io.Reader, sizeHint int64) error {
	put := &s3.PutObjectInput{
		Bucket:      aws.String(c.Bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	}
	if sizeHint > 0 {
		put.ContentLength = aws.Int64(sizeHint)
	}
	_, err := c.S3.PutObject(ctx, put)
	if err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}
	return nil
}

// DeleteObject removes a single key. Missing keys are not an error (S3 semantics).
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	_, err := c.S3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

// PresignGet returns a time-limited GET URL — used for inline <audio>
// preview in admin UI and (later) for the studio music-fetch flow.
func (c *Client) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	out, err := c.Presign.PresignGetObject(ctx,
		&s3.GetObjectInput{
			Bucket: aws.String(c.Bucket),
			Key:    aws.String(key),
		},
		s3.WithPresignExpires(ttl),
	)
	if err != nil {
		return "", fmt.Errorf("presign GET %q: %w", key, err)
	}
	return out.URL, nil
}

// PresignPut returns a time-limited PUT URL — used for studio uploads of
// rendered deliverables (Phase 7.1). The URL signs (bucket, key, content-type),
// so the studio MUST pass the same Content-Type header on its PUT or the
// signature will mismatch.
func (c *Client) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	put := &s3.PutObjectInput{
		Bucket: aws.String(c.Bucket),
		Key:    aws.String(key),
	}
	if contentType != "" {
		put.ContentType = aws.String(contentType)
	}
	out, err := c.Presign.PresignPutObject(ctx, put, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign PUT %q: %w", key, err)
	}
	return out.URL, nil
}

// HeadETag returns the S3 ETag for an object (sans surrounding quotes). Used
// as a content-version stamp by clients that want to cache downloaded blobs
// across runs — when the ETag changes, the cached copy is stale.
//
// Returns "" + nil error if the object doesn't exist (404), so callers can
// distinguish "missing slot" from "S3 unreachable".
func (c *Client) HeadETag(ctx context.Context, key string) (string, error) {
	out, err := c.S3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var resp *smithyhttp.ResponseError
		if errors.As(err, &resp) && resp.HTTPStatusCode() == 404 {
			return "", nil
		}
		return "", fmt.Errorf("head %q: %w", key, err)
	}
	if out.ETag == nil {
		return "", nil
	}
	// S3 ETags arrive wrapped in quotes — "\"abc123\"". Strip them so callers
	// can use the value as a flat string.
	return strings.Trim(*out.ETag, `"`), nil
}

// =====================================================================
// helpers
// =====================================================================
// isAlreadyOwned checks for the S3-cross-backend "you already own this bucket"
// signal. AWS uses BucketAlreadyOwnedByYou; MinIO returns 409 with similar
// payload. Cleanest portable test is the HTTP status code.
func isAlreadyOwned(err error) bool {
	if err == nil {
		return false
	}
	var resp *smithyhttp.ResponseError
	if errors.As(err, &resp) && resp.HTTPStatusCode() == 409 {
		return true
	}
	return false
}
