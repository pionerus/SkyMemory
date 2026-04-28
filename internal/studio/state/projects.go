package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Project mirrors one row in the local `projects` table. Times are RFC 3339 UTC
// strings on disk (SQLite has no native TIMESTAMPTZ); we hydrate to time.Time on read.
type Project struct {
	ID                int64
	RemoteJumpID      int64
	RemoteClientID    int64
	AccessCode        string
	Status            string
	ClientName        string
	ClientEmail       string
	ClientPhone       string
	Output1080p       bool
	Output4K          bool
	OutputVertical    bool
	OutputPhotos      bool
	HasOperatorPhotos bool

	// Picked music track. ID 0 = none yet. Title/Artist/Duration are denormalised
	// snapshots so the UI can render offline.
	MusicTrackID    int64
	MusicTitle      string
	MusicArtist     string
	MusicDurationS  float64

	CreatedAt time.Time
	UpdatedAt time.Time
	Archived  bool
}

// ErrNotFound is returned when a Project lookup misses.
var ErrNotFound = errors.New("project not found")

// CreateProject inserts a new project row. Called right after a successful
// /api/v1/jumps/register cloud call. Returns the generated local ID.
func (db *DB) CreateProject(ctx context.Context, p Project) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO projects (
			remote_jump_id, remote_client_id, access_code, status,
			client_name, client_email, client_phone,
			output_1080p, output_4k, output_vertical, output_photos,
			has_operator_photos
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
	`,
		nullInt(p.RemoteJumpID), nullInt(p.RemoteClientID), nullStr(p.AccessCode), strOrDefault(p.Status, "draft"),
		p.ClientName, nullStr(p.ClientEmail), nullStr(p.ClientPhone),
		boolInt(p.Output1080p), boolInt(p.Output4K), boolInt(p.OutputVertical), boolInt(p.OutputPhotos),
		boolInt(p.HasOperatorPhotos),
	)
	if err != nil {
		return 0, fmt.Errorf("insert project: %w", err)
	}
	return res.LastInsertId()
}

// projectColumns is the canonical SELECT list. Keep it in sync with scanProject.
const projectColumns = `id, COALESCE(remote_jump_id,0), COALESCE(remote_client_id,0),
	COALESCE(access_code,''), status,
	client_name, COALESCE(client_email,''), COALESCE(client_phone,''),
	output_1080p, output_4k, output_vertical, output_photos,
	has_operator_photos,
	COALESCE(music_track_id,0), COALESCE(music_title,''),
	COALESCE(music_artist,''), COALESCE(music_duration_s,0),
	created_at, updated_at, archived`

func scanProject(row interface {
	Scan(dst ...any) error
}) (Project, error) {
	var (
		p                                     Project
		o1080, o4k, overt, ophot, ophown, arch int
		created, updated                       string
	)
	err := row.Scan(&p.ID, &p.RemoteJumpID, &p.RemoteClientID,
		&p.AccessCode, &p.Status,
		&p.ClientName, &p.ClientEmail, &p.ClientPhone,
		&o1080, &o4k, &overt, &ophot, &ophown,
		&p.MusicTrackID, &p.MusicTitle, &p.MusicArtist, &p.MusicDurationS,
		&created, &updated, &arch,
	)
	if err != nil {
		return p, err
	}
	p.Output1080p = o1080 == 1
	p.Output4K = o4k == 1
	p.OutputVertical = overt == 1
	p.OutputPhotos = ophot == 1
	p.HasOperatorPhotos = ophown == 1
	p.Archived = arch == 1
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	return p, nil
}

// ListProjects returns active (non-archived) projects, newest first.
func (db *DB) ListProjects(ctx context.Context, includeArchived bool) ([]Project, error) {
	q := `SELECT ` + projectColumns + ` FROM projects`
	if !includeArchived {
		q += ` WHERE archived = 0`
	}
	q += ` ORDER BY updated_at DESC`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject returns one project by local id, or ErrNotFound.
func (db *DB) GetProject(ctx context.Context, id int64) (*Project, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SetProjectMusic sets (or clears, with trackID=0) the picked music track.
// title/artist/duration are denormalised so the UI works offline.
// Pass title="" + artist="" + duration=0 when clearing.
func (db *DB) SetProjectMusic(ctx context.Context, id, trackID int64, title, artist string, durationSeconds float64) error {
	res, err := db.ExecContext(ctx, `
		UPDATE projects SET
			music_track_id   = NULLIF(?, 0),
			music_title      = NULLIF(?, ''),
			music_artist     = NULLIF(?, ''),
			music_duration_s = NULLIF(?, 0),
			updated_at       = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`,
		trackID, title, artist, durationSeconds, id,
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

// =====================================================================
// helpers
// =====================================================================
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func parseTime(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", s)
	if err != nil {
		return time.Time{}
	}
	return t
}
