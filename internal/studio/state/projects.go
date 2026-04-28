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
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Archived          bool
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

// ListProjects returns active (non-archived) projects, newest first.
func (db *DB) ListProjects(ctx context.Context, includeArchived bool) ([]Project, error) {
	q := `
		SELECT id, COALESCE(remote_jump_id,0), COALESCE(remote_client_id,0),
		       COALESCE(access_code,''), status,
		       client_name, COALESCE(client_email,''), COALESCE(client_phone,''),
		       output_1080p, output_4k, output_vertical, output_photos,
		       has_operator_photos, created_at, updated_at, archived
		FROM projects`
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
		var p Project
		var (
			created, updated string
			arch             int
			o1080, o4k, overt, ophot, ophown int
		)
		if err := rows.Scan(&p.ID, &p.RemoteJumpID, &p.RemoteClientID,
			&p.AccessCode, &p.Status,
			&p.ClientName, &p.ClientEmail, &p.ClientPhone,
			&o1080, &o4k, &overt, &ophot, &ophown,
			&created, &updated, &arch,
		); err != nil {
			return nil, err
		}
		p.Output1080p = o1080 == 1
		p.Output4K = o4k == 1
		p.OutputVertical = overt == 1
		p.OutputPhotos = ophot == 1
		p.HasOperatorPhotos = ophown == 1
		p.Archived = arch == 1
		p.CreatedAt = parseTime(created)
		p.UpdatedAt = parseTime(updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject returns one project by local id, or ErrNotFound.
func (db *DB) GetProject(ctx context.Context, id int64) (*Project, error) {
	var (
		p Project
		o1080, o4k, overt, ophot, ophown int
		created, updated string
		arch             int
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, COALESCE(remote_jump_id,0), COALESCE(remote_client_id,0),
		       COALESCE(access_code,''), status,
		       client_name, COALESCE(client_email,''), COALESCE(client_phone,''),
		       output_1080p, output_4k, output_vertical, output_photos,
		       has_operator_photos, created_at, updated_at, archived
		FROM projects WHERE id = ?`,
		id,
	).Scan(&p.ID, &p.RemoteJumpID, &p.RemoteClientID,
		&p.AccessCode, &p.Status,
		&p.ClientName, &p.ClientEmail, &p.ClientPhone,
		&o1080, &o4k, &overt, &ophot, &ophown,
		&created, &updated, &arch,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Output1080p = o1080 == 1
	p.Output4K = o4k == 1
	p.OutputVertical = overt == 1
	p.OutputPhotos = ophot == 1
	p.HasOperatorPhotos = ophown == 1
	p.Archived = arch == 1
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	return &p, nil
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
