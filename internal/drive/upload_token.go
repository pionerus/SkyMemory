package drive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GetOrCreateJumpFolder returns the Drive folder ID for a specific jump,
// creating it under the operator's root if it doesn't exist yet.
// Results are cached in jump_drive_folders so repeated calls (e.g. re-renders)
// hit the DB rather than the Drive API.
//
// If rootFolderID is empty (e.g. EnsureRootFolder not yet run for this operator),
// we bootstrap it here and save it back.
func (c *Client) GetOrCreateJumpFolder(
	ctx context.Context,
	accessToken string,
	operatorID, jumpID int64,
	rootFolderID string,
) (string, error) {
	// 1. Cache hit: check jump_drive_folders.
	var cachedFolderID string
	err := c.db.QueryRow(ctx,
		`SELECT drive_folder_id FROM jump_drive_folders WHERE jump_id = $1`,
		jumpID,
	).Scan(&cachedFolderID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("drive: query jump folder cache: %w", err)
	}
	if cachedFolderID != "" {
		if ok, _ := c.fileExists(ctx, accessToken, cachedFolderID); ok {
			return cachedFolderID, nil
		}
	}

	// 2. Bootstrap root folder if needed.
	if rootFolderID == "" {
		id, err := c.EnsureRootFolder(ctx, accessToken, "")
		if err != nil {
			return "", fmt.Errorf("drive: ensure root folder: %w", err)
		}
		rootFolderID = id
		_ = c.SaveRootFolderID(ctx, operatorID, rootFolderID)
	}

	// 3. Build human-readable folder name from jump data.
	var clientName string
	var createdAt time.Time
	err = c.db.QueryRow(ctx,
		`SELECT c.name, j.created_at
		   FROM jumps j
		   JOIN clients c ON c.id = j.client_id
		  WHERE j.id = $1`,
		jumpID,
	).Scan(&clientName, &createdAt)
	if err != nil {
		return "", fmt.Errorf("drive: lookup jump info: %w", err)
	}
	folderName := clientName + " — " + createdAt.Format("2 Jan 2006")

	// 4. Create the folder on Drive.
	newID, err := c.EnsureJumpFolder(ctx, accessToken, rootFolderID, folderName, "")
	if err != nil {
		return "", fmt.Errorf("drive: create jump folder %q: %w", folderName, err)
	}

	// 5. Cache for future calls.
	_, _ = c.db.Exec(ctx, `
		INSERT INTO jump_drive_folders (jump_id, operator_id, drive_folder_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (jump_id) DO UPDATE SET drive_folder_id = EXCLUDED.drive_folder_id`,
		jumpID, operatorID, newID,
	)

	return newID, nil
}
