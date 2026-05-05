package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Generation represents one row in the local `generations` table — the audit
// of a single Generate-button click. Status lifecycle: queued → trimming →
// concating → done (or failed at any point).
type Generation struct {
	ID          int64
	ProjectID   int64
	Status      string // queued | trimming | concating | done | failed
	ProgressPct int
	StepLabel   string
	OutputPath  string
	OutputSize  int64
	Error       string
	StartedAt   time.Time
	FinishedAt  *time.Time
	// Phase 5 deliverables — '' | 'rendering' | 'ready' | 'skipped' | 'failed'.
	Phase5Insta  string
	Phase5WOW    string
	Phase5Photos string
	// 0..100 progress; only meaningful while status is 'rendering'. UI uses
	// these to drive a thin progress bar per deliverable.
	Phase5InstaPct  int
	Phase5WOWPct    int
	Phase5PhotosPct int
}

// Phase 5 status values.
const (
	Phase5StatusRendering = "rendering"
	Phase5StatusReady     = "ready"
	Phase5StatusSkipped   = "skipped"
	Phase5StatusFailed    = "failed"
)

// Status enum constants — keep in sync with the CHECK in schema v5 (none yet,
// but the pipeline only ever writes these).
const (
	GenStatusQueued    = "queued"
	GenStatusTrimming  = "trimming"
	GenStatusConcating = "concating"
	GenStatusDone      = "done"
	GenStatusFailed    = "failed"
)

// CreateGeneration inserts a new pending row, returns its id. Pipeline
// goroutine then UpdateGeneration's it as it progresses.
func (db *DB) CreateGeneration(ctx context.Context, projectID int64) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO generations (project_id, status) VALUES (?, ?)`,
		projectID, GenStatusQueued,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateGeneration patches one or more fields on a generation row. Use the
// helper Patch types so callers can send only what changed.
type GenerationPatch struct {
	Status          *string
	ProgressPct     *int
	StepLabel       *string
	OutputPath      *string
	OutputSize      *int64
	Error           *string
	Phase5Insta     *string
	Phase5WOW       *string
	Phase5Photos    *string
	Phase5InstaPct  *int
	Phase5WOWPct    *int
	Phase5PhotosPct *int
	Finish          bool // when true, sets finished_at = now()
}

func (db *DB) UpdateGeneration(ctx context.Context, id int64, p GenerationPatch) error {
	q := `UPDATE generations SET `
	args := []any{}
	first := true
	add := func(col string, v any) {
		if !first {
			q += ", "
		}
		q += col + " = ?"
		args = append(args, v)
		first = false
	}
	if p.Status != nil       { add("status",        *p.Status) }
	if p.ProgressPct != nil  { add("progress_pct",  *p.ProgressPct) }
	if p.StepLabel != nil    { add("step_label",    *p.StepLabel) }
	if p.OutputPath != nil   { add("output_path",   *p.OutputPath) }
	if p.OutputSize != nil   { add("output_size",   *p.OutputSize) }
	if p.Error != nil        { add("error",         *p.Error) }
	if p.Phase5Insta != nil     { add("phase5_insta",      *p.Phase5Insta) }
	if p.Phase5WOW != nil       { add("phase5_wow",        *p.Phase5WOW) }
	if p.Phase5Photos != nil    { add("phase5_photos",     *p.Phase5Photos) }
	if p.Phase5InstaPct != nil  { add("phase5_insta_pct",  *p.Phase5InstaPct) }
	if p.Phase5WOWPct != nil    { add("phase5_wow_pct",    *p.Phase5WOWPct) }
	if p.Phase5PhotosPct != nil { add("phase5_photos_pct", *p.Phase5PhotosPct) }
	if p.Finish {
		add("finished_at", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	if first {
		return nil // nothing to patch
	}
	q += ` WHERE id = ?`
	args = append(args, id)

	_, err := db.ExecContext(ctx, q, args...)
	return err
}

// MarkStaleGenerationsFailed flips any generations still in an in-progress
// status (queued / trimming / concating) into failed with a "studio
// restarted" error. Called once on studio boot so a previous crash or kill
// doesn't leave rows that confuse the UI ("running" forever).
func (db *DB) MarkStaleGenerationsFailed(ctx context.Context) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE generations
		SET status = ?, error = ?, finished_at = ?
		WHERE status IN (?, ?, ?)`,
		GenStatusFailed,
		"studio restarted while this generation was running",
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		GenStatusQueued, GenStatusTrimming, GenStatusConcating,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetLatestGeneration returns the newest generation row for a project, or
// (nil, ErrNotFound) if none yet.
func (db *DB) GetLatestGeneration(ctx context.Context, projectID int64) (*Generation, error) {
	var g Generation
	var (
		stepLabel, outputPath, errStr sql.NullString
		outputSize                    sql.NullInt64
		started                       string
		finished                      sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, project_id, status, progress_pct,
		       step_label, output_path, output_size, error,
		       started_at, finished_at,
		       phase5_insta, phase5_wow, phase5_photos,
		       phase5_insta_pct, phase5_wow_pct, phase5_photos_pct
		FROM generations
		WHERE project_id = ?
		ORDER BY started_at DESC, id DESC
		LIMIT 1`, projectID,
	).Scan(&g.ID, &g.ProjectID, &g.Status, &g.ProgressPct,
		&stepLabel, &outputPath, &outputSize, &errStr,
		&started, &finished,
		&g.Phase5Insta, &g.Phase5WOW, &g.Phase5Photos,
		&g.Phase5InstaPct, &g.Phase5WOWPct, &g.Phase5PhotosPct,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.StepLabel = stepLabel.String
	g.OutputPath = outputPath.String
	g.OutputSize = outputSize.Int64
	g.Error = errStr.String
	g.StartedAt = parseTime(started)
	if finished.Valid {
		t := parseTime(finished.String)
		g.FinishedAt = &t
	}
	return &g, nil
}
