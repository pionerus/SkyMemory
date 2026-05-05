package state

import (
	"context"
	"errors"
	"time"
)

// PhotoMark is an operator-set timestamp on a clip's timeline that the
// photo-pack pipeline turns into one extracted JPEG. See migration v10.
type PhotoMark struct {
	ID        int64
	ClipID    int64
	TSeconds  float64
	CreatedAt time.Time
}

// ListPhotoMarks returns marks on `clipID` sorted by time. Empty slice +
// nil error when there are none — callers should fall back to auto-pick.
func (db *DB) ListPhotoMarks(ctx context.Context, clipID int64) ([]PhotoMark, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, clip_id, t_seconds, created_at
		   FROM photo_marks
		  WHERE clip_id = ?
		  ORDER BY t_seconds ASC`,
		clipID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PhotoMark
	for rows.Next() {
		var m PhotoMark
		var created string
		if err := rows.Scan(&m.ID, &m.ClipID, &m.TSeconds, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreatePhotoMark inserts a new mark. UNIQUE(clip_id, t_seconds) means a
// duplicate at the exact same timestamp returns an error — callers should
// treat that as "already there" and ignore.
func (db *DB) CreatePhotoMark(ctx context.Context, clipID int64, tSeconds float64) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO photo_marks (clip_id, t_seconds) VALUES (?, ?)`,
		clipID, tSeconds,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeletePhotoMark removes a mark by id. Returns ErrNotFound when the row
// doesn't exist (idempotent UI: clicking the same dot twice is fine).
func (db *DB) DeletePhotoMark(ctx context.Context, id int64) error {
	res, err := db.ExecContext(ctx, `DELETE FROM photo_marks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// IsDuplicateMark recognises the SQLite UNIQUE-violation that bubbles up
// from CreatePhotoMark when an operator double-clicks the same spot.
func IsDuplicateMark(err error) bool {
	if err == nil {
		return false
	}
	// modernc.org/sqlite formats it as "UNIQUE constraint failed: photo_marks.clip_id, photo_marks.t_seconds"
	return errors.Is(err, ErrNotFound) || containsAll(err.Error(), "UNIQUE", "photo_marks")
}

func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !contains(s, n) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
