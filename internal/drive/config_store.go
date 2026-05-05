package drive

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConfigRow mirrors operator_drive_configs. Returned by Get when the
// operator has connected; nil + ErrNotConfigured otherwise.
type ConfigRow struct {
	OperatorID            int64
	TenantID              int64
	GoogleAccountEmail    string
	GoogleAccountID       string
	RefreshTokenEnc       []byte
	AccessTokenCache      string
	AccessTokenExpiresAt  time.Time
	RootFolderID          string
	Scopes                string
	LastHealthCheckAt     time.Time
	LastHealthCheckOK     bool
	LastHealthCheckError  string
}

// ErrNotConfigured is what GetConfig returns when an operator hasn't
// connected Drive yet. Handlers translate this into the "not connected"
// view on /operator/storage and into a fallback-to-S3 path in the upload
// flow.
var ErrNotConfigured = errors.New("drive: operator has not connected google drive")

// GetConfig loads one operator's drive config. Tenant scoping is enforced
// by callers (we don't take tenant_id as a parameter; the row itself is
// keyed by operator_id which only one tenant can own).
func (c *Client) GetConfig(ctx context.Context, operatorID int64) (*ConfigRow, error) {
	var (
		row                ConfigRow
		accessCache        *string
		accessExpires      *time.Time
		rootFolder         *string
		healthAt           *time.Time
		healthOK           *bool
		healthErr          *string
		googleAccountID    *string
	)
	err := c.db.QueryRow(ctx, `
		SELECT operator_id, tenant_id,
		       google_account_email, google_account_id,
		       refresh_token_enc, access_token_cache, access_token_expires_at,
		       root_folder_id, scopes,
		       last_health_check_at, last_health_check_ok, last_health_check_error
		FROM operator_drive_configs
		WHERE operator_id = $1`,
		operatorID,
	).Scan(
		&row.OperatorID, &row.TenantID,
		&row.GoogleAccountEmail, &googleAccountID,
		&row.RefreshTokenEnc, &accessCache, &accessExpires,
		&rootFolder, &row.Scopes,
		&healthAt, &healthOK, &healthErr,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, err
	}
	if googleAccountID != nil {
		row.GoogleAccountID = *googleAccountID
	}
	if accessCache != nil {
		row.AccessTokenCache = *accessCache
	}
	if accessExpires != nil {
		row.AccessTokenExpiresAt = *accessExpires
	}
	if rootFolder != nil {
		row.RootFolderID = *rootFolder
	}
	if healthAt != nil {
		row.LastHealthCheckAt = *healthAt
	}
	if healthOK != nil {
		row.LastHealthCheckOK = *healthOK
	}
	if healthErr != nil {
		row.LastHealthCheckError = *healthErr
	}
	return &row, nil
}

// UpsertConfig writes a fresh OAuth result to the table. Called from the
// callback handler after a successful token exchange. Empty rootFolderID
// is OK on first connect — Phase B's folder bootstrap fills it in.
func (c *Client) UpsertConfig(ctx context.Context, r ConfigRow) error {
	_, err := c.db.Exec(ctx, `
		INSERT INTO operator_drive_configs
		  (operator_id, tenant_id, google_account_email, google_account_id,
		   refresh_token_enc, scopes, root_folder_id)
		VALUES ($1, $2, $3, NULLIF($4,''), $5, $6, NULLIF($7,''))
		ON CONFLICT (operator_id) DO UPDATE SET
			google_account_email = EXCLUDED.google_account_email,
			google_account_id    = COALESCE(EXCLUDED.google_account_id, operator_drive_configs.google_account_id),
			refresh_token_enc    = EXCLUDED.refresh_token_enc,
			scopes               = EXCLUDED.scopes,
			root_folder_id       = COALESCE(EXCLUDED.root_folder_id, operator_drive_configs.root_folder_id),
			updated_at           = NOW()`,
		r.OperatorID, r.TenantID, r.GoogleAccountEmail, r.GoogleAccountID,
		r.RefreshTokenEnc, r.Scopes, r.RootFolderID,
	)
	return err
}

// Delete removes the operator's config. Called from the disconnect handler
// after the refresh token has been revoked at Google's end. Files already
// uploaded stay on the operator's Drive (they own them).
func (c *Client) Delete(ctx context.Context, operatorID int64) error {
	_, err := c.db.Exec(ctx, `DELETE FROM operator_drive_configs WHERE operator_id = $1`, operatorID)
	return err
}

// SaveAccessTokenCache persists a freshly-minted access token + its
// expiry. Lets subsequent calls within the hour skip the refresh round
// trip to Google.
func (c *Client) SaveAccessTokenCache(ctx context.Context, operatorID int64, accessToken string, expiresAt time.Time) error {
	_, err := c.db.Exec(ctx,
		`UPDATE operator_drive_configs
		    SET access_token_cache = $2, access_token_expires_at = $3, updated_at = NOW()
		  WHERE operator_id = $1`,
		operatorID, accessToken, expiresAt,
	)
	return err
}

// SaveHealth writes the result of a connectivity test.
func (c *Client) SaveHealth(ctx context.Context, operatorID int64, ok bool, errMsg string) error {
	_, err := c.db.Exec(ctx,
		`UPDATE operator_drive_configs
		    SET last_health_check_at    = NOW(),
		        last_health_check_ok    = $2,
		        last_health_check_error = NULLIF($3, ''),
		        updated_at              = NOW()
		  WHERE operator_id = $1`,
		operatorID, ok, errMsg,
	)
	return err
}

// SaveRootFolderID is called once we've created/found the operator's
// "Skydive Memory" root folder on Drive.
func (c *Client) SaveRootFolderID(ctx context.Context, operatorID int64, folderID string) error {
	_, err := c.db.Exec(ctx,
		`UPDATE operator_drive_configs
		    SET root_folder_id = $2, updated_at = NOW()
		  WHERE operator_id = $1`,
		operatorID, folderID,
	)
	return err
}
