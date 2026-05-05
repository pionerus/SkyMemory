// Package state owns the studio's local SQLite database (~/.freefall-studio/state.db).
// It tracks projects (one per jump), clips, photos, and upload progress — everything
// the operator needs to resume work after restart.
//
// We use modernc.org/sqlite (pure Go, no cgo) so studio.exe stays a single static
// binary on Windows without requiring gcc/MinGW for a build.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB so we can hang typed helpers off it (CreateProject, ListProjects, …).
type DB struct {
	*sql.DB
	path string
}

// Open opens (or creates) state.db at the given path, applies the latest schema
// migration, and returns a ready-to-use handle. Path's parent directory is
// created if missing — operators don't need to mkdir manually.
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}

	// _pragma=foreign_keys(1) — explicit FK enforcement (off by default in SQLite).
	// _pragma=journal_mode(wal) — Write-Ahead Log: better concurrency.
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	db := &DB{DB: sqlDB, path: path}
	if err := db.migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// Path returns the location of the state.db file (used by the UI for diagnostics).
func (db *DB) Path() string { return db.path }

// migrate applies the schema. Idempotent — safe to call on every boot.
// We track applied versions in a tiny `meta` table so that future schema changes
// can be appended without breaking older state.db files in the wild.
func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)
	`); err != nil {
		return err
	}

	current, err := db.schemaVersion(ctx)
	if err != nil {
		return err
	}

	// schemaSteps is a map; iterate in numeric order so v1 always runs before v2.
	versions := make([]int, 0, len(schemaSteps))
	for v := range schemaSteps {
		versions = append(versions, v)
	}
	sort.Ints(versions)

	for _, v := range versions {
		if v <= current {
			continue
		}
		if _, err := db.ExecContext(ctx, schemaSteps[v]); err != nil {
			return fmt.Errorf("apply schema v%d: %w", v, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO meta(key, value) VALUES('schema_version', ?)`,
			fmt.Sprintf("%d", v),
		); err != nil {
			return fmt.Errorf("record schema v%d: %w", v, err)
		}
	}
	return nil
}

func (db *DB) schemaVersion(ctx context.Context) (int, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var n int
	_, err = fmt.Sscanf(v, "%d", &n)
	return n, err
}

// schemaSteps is an ordered append-only list. Each step is one self-contained SQL
// blob applied when the local DB is below that version. Add new steps; never edit
// past ones — they may have run on operator machines already.
var schemaSteps = map[int]string{
	1: `
		CREATE TABLE projects (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			remote_jump_id  INTEGER,                          -- jump_id assigned by cloud /api/v1/jumps/register
			remote_client_id INTEGER,                          -- client_id likewise
			access_code     TEXT,                             -- formatted XXXX-XXXX (returned by cloud)
			status          TEXT NOT NULL DEFAULT 'draft',
				-- draft | encoding | uploading | done | sent | failed
			client_name     TEXT NOT NULL,
			client_email    TEXT,
			client_phone    TEXT,
			output_1080p    INTEGER NOT NULL DEFAULT 1,
			output_4k       INTEGER NOT NULL DEFAULT 0,
			output_vertical INTEGER NOT NULL DEFAULT 0,
			output_photos   INTEGER NOT NULL DEFAULT 0,
			has_operator_photos INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			archived        INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX idx_projects_updated ON projects(updated_at DESC);
	`,

	2: `
		CREATE TABLE clips (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			kind            TEXT NOT NULL,
				-- canonical: intro | interview_pre | walk | interview_plane | freefall | landing | closing
				-- custom:    custom:<label>
			source_path     TEXT NOT NULL,                    -- absolute local path on operator's disk
			source_filename TEXT NOT NULL,                    -- original filename for display
			source_size_bytes INTEGER NOT NULL,
			source_sha256   TEXT,                             -- not yet computed; reserved for dedup
			duration_seconds REAL,                            -- ffprobe format.duration
			codec           TEXT,                             -- ffprobe streams[0].codec_name
			width           INTEGER,
			height          INTEGER,
			fps             REAL,                             -- numeric form of streams[0].r_frame_rate
			has_audio       INTEGER NOT NULL DEFAULT 0,
			audio_codec     TEXT,
			created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			UNIQUE(project_id, kind)                          -- one clip per slot per project; replace = upsert
		);
		CREATE INDEX idx_clips_project ON clips(project_id);
	`,

	3: `
		-- Trim window in seconds (manual or auto-suggested). NULL means "use full clip"
		-- so that older rows pre-dating this column degrade gracefully.
		ALTER TABLE clips ADD COLUMN trim_in_seconds  REAL;
		ALTER TABLE clips ADD COLUMN trim_out_seconds REAL;
		-- True when the values came from auto-detection (silencedetect / motion magnitude),
		-- false when operator dragged the sliders. UI surfaces an "AI suggested" badge for the former.
		ALTER TABLE clips ADD COLUMN trim_auto_suggested INTEGER NOT NULL DEFAULT 0;
	`,

	4: `
		-- Picked music track id. References cloud's music_tracks.id; we don't have
		-- foreign keys here because the catalog lives in the cloud DB, not in state.db.
		-- We additionally stash a denormalised copy of title/artist/duration so the
		-- project list can render without a cloud round-trip if offline.
		ALTER TABLE projects ADD COLUMN music_track_id   INTEGER;
		ALTER TABLE projects ADD COLUMN music_title      TEXT;
		ALTER TABLE projects ADD COLUMN music_artist     TEXT;
		ALTER TABLE projects ADD COLUMN music_duration_s REAL;
	`,

	5: `
		-- Each Generate run is one row. We keep history (audit + retry) rather than
		-- overwriting; the UI shows the latest by created_at. status enum mirrors
		-- the lifecycle steps the pipeline reports as it progresses.
		CREATE TABLE generations (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			status       TEXT NOT NULL DEFAULT 'queued',
				-- queued | trimming | concating | done | failed
			progress_pct INTEGER NOT NULL DEFAULT 0,
			step_label   TEXT,                         -- 'trimming intro', 'concat', 'final encode'
			output_path  TEXT,                         -- filled when status='done'
			output_size  INTEGER,
			error        TEXT,                         -- non-null when status='failed'
			started_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			finished_at  TEXT
		);
		CREATE INDEX idx_generations_project ON generations(project_id, started_at DESC);
	`,

	6: `
		-- Cut / exclude zones inside a clip's trim window. Operator paints over
		-- moments that should be removed from the final render (operator walked
		-- into frame, jumper sneezed, etc). Pipeline turns N cut zones into
		-- N+1 trim segments + concat in the filter graph.
		--
		-- start_seconds and end_seconds are in source-clip seconds, same scale
		-- as clips.trim_in_seconds. They MUST sit inside the trim window, but
		-- this is enforced application-side, not by CHECK (so AI-suggested
		-- cuts can sit outside while we tune them).
		CREATE TABLE clip_cuts (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			clip_id        INTEGER NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
			start_seconds  REAL NOT NULL,
			end_seconds    REAL NOT NULL,
			reason         TEXT,                       -- 'operator-in-frame', 'silence', 'manual', etc
			auto_suggested INTEGER NOT NULL DEFAULT 0, -- 1 when added by silence-detect / motion-magnitude
			created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			UNIQUE(clip_id, start_seconds)
		);
		CREATE INDEX idx_clip_cuts_clip ON clip_cuts(clip_id, start_seconds);
	`,

	7: `
		-- Speech-start marker (in source-clip seconds, same scale as trim_in_seconds).
		-- Set on action clips that contain a post-action interview at the tail —
		-- typical case is landing footage where the jumper turns to camera and
		-- starts talking after the canopy lands. Pipeline behaviour:
		--
		--   trim_in   →  speech_start_seconds  → action portion (silent, music plays)
		--   speech_start_seconds  →  trim_out  → interview portion (keep audio,
		--                                         music ducks via sidechain)
		--
		-- NULL = no marker, clip behaves per its kind's normal heuristic
		-- (action kinds = silent, interview kinds = full interview audio).
		ALTER TABLE clips ADD COLUMN speech_start_seconds REAL;
	`,

	8: `
		-- Phase 5 deliverables status — surfaces per-deliverable progress on
		-- the generate page so the operator can tell at a glance which short-
		-- form pieces succeeded vs. were silently skipped (segment picker
		-- returned ok=false, etc).
		--
		-- Values: '' (not started), 'rendering', 'ready', 'skipped', 'failed'.
		-- 'skipped' = picker bailed (e.g. freefall too short for a WOW reel).
		-- 'failed'  = render or upload errored.
		ALTER TABLE generations ADD COLUMN phase5_insta  TEXT NOT NULL DEFAULT '';
		ALTER TABLE generations ADD COLUMN phase5_wow    TEXT NOT NULL DEFAULT '';
		ALTER TABLE generations ADD COLUMN phase5_photos TEXT NOT NULL DEFAULT '';
	`,

	9: `
		-- Per-deliverable progress percentage (0..100). Set by the runner as
		-- ffmpeg reports out_time_us / total duration; the photo-pack flavour
		-- increments by 1/N per extracted frame. Lets the generate page show
		-- a real progress bar instead of an indeterminate "rendering" pill.
		ALTER TABLE generations ADD COLUMN phase5_insta_pct  INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE generations ADD COLUMN phase5_wow_pct    INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE generations ADD COLUMN phase5_photos_pct INTEGER NOT NULL DEFAULT 0;
	`,

	10: `
		-- Operator-curated photo timestamps on a clip's timeline. Used by
		-- the photo-pack pipeline: if any marks exist on the freefall clip
		-- the planner uses them as anchors and only auto-fills the slack
		-- (up to 20 photos total). When no marks exist the planner falls
		-- back to the previous all-auto-distributed behaviour.
		--
		-- t_seconds is in source-clip seconds, same scale as trim_in_seconds.
		-- Constrained to live inside the trim window application-side
		-- (no CHECK so an AI suggestion outside the window can be tuned).
		CREATE TABLE photo_marks (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			clip_id     INTEGER NOT NULL REFERENCES clips(id) ON DELETE CASCADE,
			t_seconds   REAL NOT NULL,
			created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			UNIQUE(clip_id, t_seconds)
		);
		CREATE INDEX idx_photo_marks_clip ON photo_marks(clip_id, t_seconds);
	`,

	11: `
		-- Per-project ordering of clips. Drives both the slot order on the
		-- clip board and the order clips are concatenated by the pipeline.
		-- Stored as an integer with gaps (10, 20, 30, ...) so reordering
		-- requires updating only the moved row in the common case.
		ALTER TABLE clips ADD COLUMN position INTEGER NOT NULL DEFAULT 0;

		-- Backfill: canonical kinds get their conventional order; custom
		-- ones land after, in creation order (id ascending).
		UPDATE clips SET position = CASE kind
			WHEN 'intro'           THEN 10
			WHEN 'interview_pre'   THEN 20
			WHEN 'walk'            THEN 30
			WHEN 'interview_plane' THEN 40
			WHEN 'freefall'        THEN 50
			WHEN 'landing'         THEN 60
			WHEN 'closing'         THEN 70
			ELSE 1000 + id
		END;

		CREATE INDEX idx_clips_project_position ON clips(project_id, position);
	`,
}
