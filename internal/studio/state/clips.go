package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Canonical segment kinds. Operator-added custom segments use a "custom:" prefix.
//
// KindIntro / KindClosing are RETAINED as constants (so legacy projects with
// already-uploaded clips of those kinds still scan/load) but they're no
// longer in CanonicalKinds — the operator's clip board only exposes the
// 5 jump-specific segments. Branding intro/outro is configured by the
// club admin on /admin/branding and concatenated by the pipeline
// automatically (see runner.go § Intro/outro normalise + concat).
const (
	KindIntro          = "intro"   // legacy — replaced by club-admin branding.intro
	KindInterviewPre   = "interview_pre"
	KindWalk           = "walk"
	KindInterviewPlane = "interview_plane"
	KindFreefall       = "freefall"
	KindLanding        = "landing"
	KindClosing        = "closing" // legacy — replaced by club-admin branding.outro
	CustomPrefix       = "custom:"
)

// CanonicalKinds returns the 5 jump-specific slots the operator films.
// Logo, intro and closing live on /admin/branding (club admin) and the
// pipeline merges them at render time. project_detail.html iterates this
// for the slot grid.
func CanonicalKinds() []string {
	return []string{
		KindInterviewPre, KindWalk,
		KindInterviewPlane, KindFreefall, KindLanding,
	}
}

// IsLegacyBrandingKind reports kinds that used to be operator-uploaded
// but are now sourced from the club admin's branding bundle. Pipeline
// skips these so old projects don't double-render the intro/outro.
func IsLegacyBrandingKind(kind string) bool {
	return kind == KindIntro || kind == KindClosing
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
	// Trim window — operator-set or auto-suggested. Both NULL on rows older than schema v3
	// (we map NULL -> 0 here; callers compare TrimOutSeconds==0 to detect "use full clip").
	TrimInSeconds     float64
	TrimOutSeconds    float64
	TrimAutoSuggested bool

	// Speech-start marker (schema v7). Source-clip seconds; NULL/<=0 means
	// "no marker, use the kind's normal heuristic". When set, the pipeline
	// splits the clip at this offset: action portion before, interview
	// portion after (keep audio + sidechain music). HasSpeechStart() checks.
	SpeechStartSeconds float64

	// Per-project ordering — drives both the slot grid on the clip board and
	// the order clips are concatenated in the rendered video. Canonical kinds
	// get conventional defaults (10..70) on first insert; ReorderClips
	// rewrites positions when the operator drags a slot around.
	Position int

	CreatedAt time.Time
}

// HasSpeechStart returns true when the operator (or auto-detect) has placed
// a marker inside the trim window — telling the pipeline this clip carries
// post-action interview audio after the marker timestamp.
func (c *Clip) HasSpeechStart() bool {
	if c.SpeechStartSeconds <= 0 {
		return false
	}
	tIn := c.TrimInSeconds
	tOut := c.EffectiveTrimOut()
	return c.SpeechStartSeconds > tIn+0.05 && c.SpeechStartSeconds < tOut-0.05
}

// HasTrim returns true if a non-default trim window has been set on this clip.
func (c *Clip) HasTrim() bool {
	if c.TrimOutSeconds <= 0 {
		return false
	}
	if c.TrimInSeconds == 0 && c.DurationSeconds > 0 && c.TrimOutSeconds >= c.DurationSeconds-0.01 {
		return false // (0, full duration) == no real trim
	}
	return true
}

// EffectiveTrimOut returns trim_out_seconds if set, else duration.
// Used for rendering "trimmed: X→Y" pills and for the timeline UI.
func (c *Clip) EffectiveTrimOut() float64 {
	if c.TrimOutSeconds > 0 {
		return c.TrimOutSeconds
	}
	return c.DurationSeconds
}

// UpsertClip inserts a new clip or replaces an existing one for the same (project_id, kind).
// Returns the resulting row's id. NEW clips get a `position` chosen by
// nextClipPosition (canonical defaults for known kinds, max+10 for custom).
// EXISTING clips keep their existing position so the operator's reorder isn't
// clobbered when they replace a file.
func (db *DB) UpsertClip(ctx context.Context, c Clip) (int64, error) {
	pos, err := db.nextClipPosition(ctx, c.ProjectID, c.Kind)
	if err != nil {
		return 0, fmt.Errorf("compute position: %w", err)
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO clips (
			project_id, kind, source_path, source_filename, source_size_bytes,
			source_sha256, duration_seconds, codec, width, height, fps,
			has_audio, audio_codec, position
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
			-- position deliberately NOT overwritten on conflict
	`,
		c.ProjectID, c.Kind, c.SourcePath, c.SourceFilename, c.SourceSizeBytes,
		nullStr(c.SourceSHA256), c.DurationSeconds, nullStr(c.Codec),
		c.Width, c.Height, c.FPS,
		boolInt(c.HasAudio), nullStr(c.AudioCodec), pos,
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

// clipColumns is the canonical SELECT list for clips. Keeping it in one place
// avoids drift between ListClips and GetClip.
const clipColumns = `id, project_id, kind, source_path, source_filename, source_size_bytes,
	COALESCE(source_sha256,''), COALESCE(duration_seconds,0), COALESCE(codec,''),
	COALESCE(width,0), COALESCE(height,0), COALESCE(fps,0),
	has_audio, COALESCE(audio_codec,''),
	COALESCE(trim_in_seconds,0), COALESCE(trim_out_seconds,0), trim_auto_suggested,
	COALESCE(speech_start_seconds,0),
	position,
	created_at`

func scanClip(row interface {
	Scan(dst ...any) error
}) (Clip, error) {
	var c Clip
	var hasAudio, trimAuto int
	var created string
	err := row.Scan(&c.ID, &c.ProjectID, &c.Kind,
		&c.SourcePath, &c.SourceFilename, &c.SourceSizeBytes,
		&c.SourceSHA256, &c.DurationSeconds, &c.Codec,
		&c.Width, &c.Height, &c.FPS,
		&hasAudio, &c.AudioCodec,
		&c.TrimInSeconds, &c.TrimOutSeconds, &trimAuto,
		&c.SpeechStartSeconds,
		&c.Position,
		&created,
	)
	if err != nil {
		return c, err
	}
	c.HasAudio = hasAudio == 1
	c.TrimAutoSuggested = trimAuto == 1
	c.CreatedAt = parseTime(created)
	return c, nil
}

// ListClips returns all clips attached to a project ordered by the operator's
// chosen sequence (clips.position). Position starts at canonical defaults
// (10, 20, 30, ...) but can be overridden via ReorderClips → drag-drop on
// the clip board → POST /projects/{id}/clips/reorder.
func (db *DB) ListClips(ctx context.Context, projectID int64) ([]Clip, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+clipColumns+`
		FROM clips
		WHERE project_id = ?
		ORDER BY position ASC, created_at ASC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Clip
	for rows.Next() {
		c, err := scanClip(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReorderClips writes a new ordering for the given clip IDs. Each ID gets
// position = (i+1)*10, leaving gaps for future single-clip moves. Clips
// not in the IDs list keep their current position. Tenant scoping is via
// the project_id check so a malformed request can't move clips between
// projects.
func (db *DB) ReorderClips(ctx context.Context, projectID int64, clipIDs []int64) error {
	if len(clipIDs) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE clips SET position = ? WHERE id = ? AND project_id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, id := range clipIDs {
		pos := (i + 1) * 10
		if _, err := stmt.ExecContext(ctx, pos, id, projectID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetClipPosition overrides the clip's position to a specific value.
// Used by smart-import after UpsertClip to interleave custom slots between
// canonical kinds (ergonomic for B-roll like clouds / plane-window shots).
func (db *DB) SetClipPosition(ctx context.Context, clipID int64, position int) error {
	_, err := db.ExecContext(ctx,
		`UPDATE clips SET position = ? WHERE id = ?`, position, clipID)
	return err
}

// nextClipPosition computes the position to assign to a brand-new clip
// (one we're inserting via UpsertClip with no existing row). Lands at
// max(existing) + 10 so canonical and previously-placed customs keep
// their order; the new one goes at the tail.
func (db *DB) nextClipPosition(ctx context.Context, projectID int64, kind string) (int, error) {
	// Canonical kinds keep their canonical default the FIRST time they're
	// inserted — that way the operator's slot grid renders in the natural
	// "interview_pre, walk, plane, freefall, landing" order on a fresh
	// project. Once the operator drags things around, ReorderClips
	// overrides this anyway.
	switch kind {
	case KindIntro:
		return 10, nil
	case KindInterviewPre:
		return 20, nil
	case KindWalk:
		return 30, nil
	case KindInterviewPlane:
		return 40, nil
	case KindFreefall:
		return 50, nil
	case KindLanding:
		return 60, nil
	case KindClosing:
		return 70, nil
	}
	var maxPos sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(position) FROM clips WHERE project_id = ?`, projectID,
	).Scan(&maxPos); err != nil {
		return 0, err
	}
	if maxPos.Valid {
		return int(maxPos.Int64) + 10, nil
	}
	return 100, nil
}

// GetClip returns one clip by (project_id, kind), or ErrNotFound.
func (db *DB) GetClip(ctx context.Context, projectID int64, kind string) (*Clip, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+clipColumns+` FROM clips WHERE project_id = ? AND kind = ?`,
		projectID, kind,
	)
	c, err := scanClip(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetClipByID returns one clip by primary id, or ErrNotFound. Used by the
// cut-resize endpoint, which only knows the cut id and needs to resolve
// the parent clip's trim window for validation.
func (db *DB) GetClipByID(ctx context.Context, clipID int64) (*Clip, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+clipColumns+` FROM clips WHERE id = ?`,
		clipID,
	)
	c, err := scanClip(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// UpdateClipTrim sets trim_in/out + auto-suggested flag for one clip.
// in must be ≥0 and < out; out must be ≤ duration (caller validates).
// trimOut == 0 is a sentinel meaning "use full duration", so we allow it.
func (db *DB) UpdateClipTrim(ctx context.Context, projectID int64, kind string, trimIn, trimOut float64, autoSuggested bool) error {
	res, err := db.ExecContext(ctx, `
		UPDATE clips
		SET trim_in_seconds  = ?,
		    trim_out_seconds = ?,
		    trim_auto_suggested = ?
		WHERE project_id = ? AND kind = ?`,
		trimIn, trimOut, boolInt(autoSuggested),
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

// UpdateClipSpeechStart sets (or clears, with t<=0) the speech-start marker
// for a clip. Validation that t sits inside the trim window is the caller's
// job — we keep the DB layer dumb.
func (db *DB) UpdateClipSpeechStart(ctx context.Context, projectID int64, kind string, t float64) error {
	var arg any = t
	if t <= 0 {
		arg = nil // clear
	}
	res, err := db.ExecContext(ctx, `
		UPDATE clips
		SET speech_start_seconds = ?
		WHERE project_id = ? AND kind = ?`,
		arg, projectID, kind,
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

// SumProjectClipDuration returns the total seconds the final video would
// take if rendered from the current set of clips: sum of (effective_trim_out -
// effective_trim_in) for every clip with metadata. Clips lacking a duration
// are skipped (we can't reason about them without ffprobe). 0 means the
// caller should treat the project as "duration unknown".
func (db *DB) SumProjectClipDuration(ctx context.Context, projectID int64) (float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(duration_seconds, 0),
		       COALESCE(trim_in_seconds,  0),
		       COALESCE(trim_out_seconds, 0)
		FROM clips
		WHERE project_id = ?`, projectID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	total := 0.0
	for rows.Next() {
		var dur, in, out float64
		if err := rows.Scan(&dur, &in, &out); err != nil {
			return 0, err
		}
		if dur <= 0 {
			continue // unprobed clip — skip rather than treat as 0-length
		}
		// trim_out == 0 is the sentinel "use full clip"
		if out <= 0 || out > dur {
			out = dur
		}
		if in < 0 {
			in = 0
		}
		if out > in {
			total += out - in
		}
	}
	return total, rows.Err()
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
