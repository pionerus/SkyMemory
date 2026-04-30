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
}
