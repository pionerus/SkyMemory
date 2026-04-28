package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Canonical segment kinds. Operator-added custom segments use a "custom:" prefix.
const (
	KindIntro          = "intro"
	KindInterviewPre   = "interview_pre"
	KindWalk           = "walk"
	KindInterviewPlane = "interview_plane"
	KindFreefall       = "freefall"
	KindLanding        = "landing"
	KindClosing        = "closing"
	CustomPrefix       = "custom:"
)

// CanonicalKinds returns the 7 default slots in canonical order.
// project_detail.html iterates this for the slot grid.
func CanonicalKinds() []string {
	return []string{
		KindIntro, KindInterviewPre, KindWalk,
		KindInterviewPlane, KindFreefall, KindLanding, KindClosing,
	}
}

// HumanKindLabel returns a friendly display label, e.g. "interview_pre" -> "Interview (pre-jump)".
func HumanKindLabel(kind string) string {
	switch kind {
	case KindIntro:
		return "Intro"
	case KindInterviewPre:
		return "Interview (pre-jump)"
	case KindWalk:
		return "Walk to plane"
	case KindInterviewPlane:
		return "Interview (in plane)"
	case KindFreefall:
		return "Freefall"
	case KindLanding:
		return "Landing"
	case KindClosing:
		return "Closing"
	}
	if len(kind) > len(CustomPrefix) && kind[:len(CustomPrefix)] == CustomPrefix {
		return "Custom: " + kind[len(CustomPrefix):]
	}
	return kind
}

// Clip mirrors one row in the clips table.
type Clip struct {
	ID              int64
	ProjectID       int64
	Kind            string
	SourcePath      string
	SourceFilename  string
	SourceSizeBytes int64
	SourceSHA256    string
	DurationSeconds float64
	Codec           string
	Width           int
	Height          int
	FPS             float64
	HasAudio        bool
	AudioCodec      string
	CreatedAt       time.Time
}

// UpsertClip inserts a new clip or replaces an existing one for the same (project_id, kind).
// Returns the resulting row's id.
func (db *DB) UpsertClip(ctx context.Context, c Clip) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO clips (
			project_id, kind, source_path, source_filename, source_size_bytes,
			source_sha256, duration_seconds, codec, width, height, fps,
			has_audio, audio_codec
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id, kind) DO UPDATE SET
			source_path       = excluded.source_path,
			source_filename   = excluded.source_filename,
			source_size_bytes = excluded.source_size_bytes,
			source_sha256     = excluded.source_sha256,
			duration_seconds  = excluded.duration_seconds,
			codec             = excluded.codec,
			width             = excluded.width,
			height            = excluded.height,
			fps               = excluded.fps,
			has_audio         = excluded.has_audio,
			audio_codec       = excluded.audio_codec
	`,
		c.ProjectID, c.Kind, c.SourcePath, c.SourceFilename, c.SourceSizeBytes,
		nullStr(c.SourceSHA256), c.DurationSeconds, nullStr(c.Codec),
		c.Width, c.Height, c.FPS,
		boolInt(c.HasAudio), nullStr(c.AudioCodec),
	)
	if err != nil {
		return 0, fmt.Errorf("upsert clip: %w", err)
	}
	if id, ierr := res.LastInsertId(); ierr == nil && id > 0 {
		return id, nil
	}

	// On UPDATE conflict path LastInsertId may be 0 — look it up explicitly.
	var id int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM clips WHERE project_id = ? AND kind = ?`,
		c.ProjectID, c.Kind,
	).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// ListClips returns all clips attached to a project, ordered by canonical kind first
// (intro → closing) then by created_at for custom ones. Useful for the project detail page.
func (db *DB) ListClips(ctx context.Context, projectID int64) ([]Clip, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, project_id, kind, source_path, source_filename, source_size_bytes,
		       COALESCE(source_sha256,''), COALESCE(duration_seconds,0), COALESCE(codec,''),
		       COALESCE(width,0), COALESCE(height,0), COALESCE(fps,0),
		       has_audio, COALESCE(audio_codec,''), created_at
		FROM clips
		WHERE project_id = ?
		ORDER BY
			CASE kind
				WHEN 'intro'           THEN 1
				WHEN 'interview_pre'   THEN 2
				WHEN 'walk'            THEN 3
				WHEN 'interview_plane' THEN 4
				WHEN 'freefall'        THEN 5
				WHEN 'landing'         THEN 6
				WHEN 'closing'         THEN 7
				ELSE 99
			END,
			created_at ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Clip
	for rows.Next() {
		var c Clip
		var hasAudio int
		var created string
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Kind,
			&c.SourcePath, &c.SourceFilename, &c.SourceSizeBytes,
			&c.SourceSHA256, &c.DurationSeconds, &c.Codec,
			&c.Width, &c.Height, &c.FPS,
			&hasAudio, &c.AudioCodec, &created,
		); err != nil {
			return nil, err
		}
		c.HasAudio = hasAudio == 1
		c.CreatedAt = parseTime(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetClip returns one clip by (project_id, kind), or ErrNotFound.
func (db *DB) GetClip(ctx context.Context, projectID int64, kind string) (*Clip, error) {
	var c Clip
	var hasAudio int
	var created string
	err := db.QueryRowContext(ctx, `
		SELECT id, project_id, kind, source_path, source_filename, source_size_bytes,
		       COALESCE(source_sha256,''), COALESCE(duration_seconds,0), COALESCE(codec,''),
		       COALESCE(width,0), COALESCE(height,0), COALESCE(fps,0),
		       has_audio, COALESCE(audio_codec,''), created_at
		FROM clips
		WHERE project_id = ? AND kind = ?`,
		projectID, kind,
	).Scan(&c.ID, &c.ProjectID, &c.Kind,
		&c.SourcePath, &c.SourceFilename, &c.SourceSizeBytes,
		&c.SourceSHA256, &c.DurationSeconds, &c.Codec,
		&c.Width, &c.Height, &c.FPS,
		&hasAudio, &c.AudioCodec, &created,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.HasAudio = hasAudio == 1
	c.CreatedAt = parseTime(created)
	return &c, nil
}

// DeleteClip removes a clip row. Caller is responsible for unlinking the underlying
// file on disk (we don't own the source — operator's path) — but we DO own the copy
// under ~/.freefall-studio/jobs/<project_id>/, see cmd/studio/main.go for cleanup.
func (db *DB) DeleteClip(ctx context.Context, projectID int64, kind string) error {
	res, err := db.ExecContext(ctx,
		`DELETE FROM clips WHERE project_id = ? AND kind = ?`,
		projectID, kind,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
