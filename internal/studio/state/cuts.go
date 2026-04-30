package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Cut is one exclusion zone inside a clip's trim window — a sub-range the
// operator wants the renderer to SKIP. Pipeline turns N cut zones into N+1
// keep-segments via split + concat in the filter graph.
type Cut struct {
	ID            int64
	ClipID        int64
	StartSeconds  float64
	EndSeconds    float64
	Reason        string // "manual" | "operator-in-frame" | "silence" | "" (unset)
	AutoSuggested bool
	CreatedAt     time.Time
}

// Length is the duration of the cut zone in seconds.
func (c Cut) Length() float64 { return c.EndSeconds - c.StartSeconds }

// CreateCut inserts a new cut row. Validation that start<end and that the
// zone fits within the clip's trim window is the caller's responsibility —
// we keep the DB layer dumb so AI suggestions can land here even when slightly
// outside the window during tuning.
func (db *DB) CreateCut(ctx context.Context, clipID int64, start, end float64, reason string, autoSuggested bool) (int64, error) {
	if end <= start {
		return 0, errors.New("cut end must be > start")
	}
	if start < 0 {
		return 0, errors.New("cut start must be >= 0")
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO clip_cuts (clip_id, start_seconds, end_seconds, reason, auto_suggested)
		VALUES (?, ?, ?, ?, ?)`,
		clipID, start, end, reason, boolToInt(autoSuggested),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeleteCut removes a cut by id. The schema's ON DELETE CASCADE on clip_id
// keeps cuts in sync with clip removals, so we don't scope by clip here.
func (db *DB) DeleteCut(ctx context.Context, cutID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM clip_cuts WHERE id = ?`, cutID)
	return err
}

// ListCuts returns all cuts for one clip, sorted by start ascending — order
// matters because the pipeline's split+concat assumes monotonically increasing
// boundaries.
func (db *DB) ListCuts(ctx context.Context, clipID int64) ([]Cut, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, clip_id, start_seconds, end_seconds, reason, auto_suggested, created_at
		FROM clip_cuts
		WHERE clip_id = ?
		ORDER BY start_seconds ASC`, clipID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cut
	for rows.Next() {
		c, err := scanCut(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCutsForProject returns cuts for all clips in a project, keyed by clip
// id. The pipeline calls this once before rendering instead of doing N+1
// queries while building the filter graph.
func (db *DB) ListCutsForProject(ctx context.Context, projectID int64) (map[int64][]Cut, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT cc.id, cc.clip_id, cc.start_seconds, cc.end_seconds,
		       cc.reason, cc.auto_suggested, cc.created_at
		FROM clip_cuts cc
		JOIN clips c ON c.id = cc.clip_id
		WHERE c.project_id = ?
		ORDER BY cc.clip_id, cc.start_seconds ASC`, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]Cut{}
	for rows.Next() {
		c, err := scanCut(rows)
		if err != nil {
			return nil, err
		}
		out[c.ClipID] = append(out[c.ClipID], c)
	}
	return out, rows.Err()
}

// GetCutClip returns the project_id that owns a given cut. Used by the
// HTTP delete handler to validate scope before honouring the request.
func (db *DB) GetCutClip(ctx context.Context, cutID int64) (clipID, projectID int64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT cc.clip_id, c.project_id
		FROM clip_cuts cc
		JOIN clips c ON c.id = cc.clip_id
		WHERE cc.id = ?`, cutID,
	).Scan(&clipID, &projectID)
	return
}

func scanCut(rows *sql.Rows) (Cut, error) {
	var (
		c          Cut
		auto       int
		createdStr string
		reason     sql.NullString
	)
	if err := rows.Scan(&c.ID, &c.ClipID, &c.StartSeconds, &c.EndSeconds, &reason, &auto, &createdStr); err != nil {
		return Cut{}, err
	}
	c.Reason = reason.String
	c.AutoSuggested = auto != 0
	c.CreatedAt = parseTime(createdStr)
	return c, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
