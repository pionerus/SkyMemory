// Package billing computes per-club monthly bills from delivered jumps +
// photo packs against the club's stored per-unit rates.
//
// Billing rules (set by Sergei, 2026-05):
//   - 1 delivered jump (any video artifact)        → tenants.video_price_cents
//   - 1 delivered photo pack (≥1 photo artifact)   → tenants.photo_pack_price_cents
//
// "Delivered" = a jump_artifacts row exists for that jump in the requested
// month. We bill by uploaded_at month so a render that completed late on
// the 1st belongs to the new month, matching what the operator's weekly
// report shows.
package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is the small subset of *db.Pool we need. Passing the interface
// keeps this package decoupled from internal/db so unit tests can stub.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Bill is the per-tenant invoice for one calendar month.
type Bill struct {
	TenantID     int64
	TenantName   string
	Year         int
	Month        time.Month
	MonthLabel   string // "May 2026"
	WindowStart  time.Time
	WindowEnd    time.Time

	VideoPriceCents int // copy of tenants.video_price_cents at compute time
	PhotoPriceCents int // copy of tenants.photo_pack_price_cents at compute time

	VideoCount      int // jumps with ≥1 video artifact uploaded in window
	PhotoPackCount  int // jumps with ≥1 photo artifact uploaded in window
	VideoTotalCents int
	PhotoTotalCents int
	GrandTotalCents int
}

// EuroVideo / EuroPhoto / EuroTotal are template-friendly accessors —
// templates can't do divisions, so we surface "12.34" strings here.
func (b Bill) EuroVideo() string  { return cents(b.VideoTotalCents) }
func (b Bill) EuroPhoto() string  { return cents(b.PhotoTotalCents) }
func (b Bill) EuroTotal() string  { return cents(b.GrandTotalCents) }
func (b Bill) EuroPerVideo() string { return cents(b.VideoPriceCents) }
func (b Bill) EuroPerPhoto() string { return cents(b.PhotoPriceCents) }

// MonthWindow returns [start, end) for a calendar month in UTC. Used both
// here and on caller sites that want to show the date range.
func MonthWindow(year int, month time.Month) (time.Time, time.Time) {
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	return start, end
}

// Compute returns the bill for one tenant + month. Numbers come from
// jump_artifacts (uploaded_at within window); rates come from the tenants
// row. Both sides scoped to tenant_id so a multi-tenant compromise can't
// pollute the bill.
func Compute(ctx context.Context, q Querier, tenantID int64, year int, month time.Month) (*Bill, error) {
	start, end := MonthWindow(year, month)

	var (
		videoCents, photoCents int
		tenantName             string
	)
	if err := q.QueryRow(ctx, `
		SELECT name, video_price_cents, photo_pack_price_cents
		FROM tenants
		WHERE id = $1
		  AND deleted_at IS NULL`, tenantID,
	).Scan(&tenantName, &videoCents, &photoCents); err != nil {
		return nil, fmt.Errorf("load tenant rates: %w", err)
	}

	var videoCount, photoPackCount int
	if err := q.QueryRow(ctx, `
		SELECT
		  COUNT(DISTINCT j.id) FILTER (
		    WHERE EXISTS (
		      SELECT 1 FROM jump_artifacts a
		      WHERE a.jump_id = j.id
		        AND a.kind IN ('horizontal_1080p','horizontal_4k','vertical','wow_highlights')
		        AND a.uploaded_at >= $2 AND a.uploaded_at < $3
		    )
		  ) AS video_count,
		  COUNT(DISTINCT j.id) FILTER (
		    WHERE EXISTS (
		      SELECT 1 FROM jump_artifacts a
		      WHERE a.jump_id = j.id
		        AND a.kind = 'photo'
		        AND a.uploaded_at >= $2 AND a.uploaded_at < $3
		    )
		  ) AS photo_pack_count
		FROM jumps j
		WHERE j.tenant_id = $1`,
		tenantID, start, end,
	).Scan(&videoCount, &photoPackCount); err != nil {
		return nil, fmt.Errorf("count jumps: %w", err)
	}

	b := &Bill{
		TenantID:        tenantID,
		TenantName:      tenantName,
		Year:            year,
		Month:           month,
		MonthLabel:      fmt.Sprintf("%s %d", month.String(), year),
		WindowStart:     start,
		WindowEnd:       end,
		VideoPriceCents: videoCents,
		PhotoPriceCents: photoCents,
		VideoCount:      videoCount,
		PhotoPackCount:  photoPackCount,
		VideoTotalCents: videoCount * videoCents,
		PhotoTotalCents: photoPackCount * photoCents,
	}
	b.GrandTotalCents = b.VideoTotalCents + b.PhotoTotalCents
	return b, nil
}

// AllClubs computes Compute() for every active tenant, oldest first. Used
// by /platform/billing for the cross-tenant table.
func AllClubs(ctx context.Context, q Querier, year int, month time.Month) ([]Bill, error) {
	rows, err := q.Query(ctx, `SELECT id FROM tenants WHERE deleted_at IS NULL ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	out := make([]Bill, 0, len(ids))
	for _, id := range ids {
		b, err := Compute(ctx, q, id, year, month)
		if err != nil {
			continue // best-effort — one bad tenant shouldn't 500 the page
		}
		out = append(out, *b)
	}
	return out, nil
}

// CurrentMonth is a convenience for callers that just want "now".
func CurrentMonth() (int, time.Month) {
	t := time.Now().UTC()
	return t.Year(), t.Month()
}

func cents(c int) string {
	if c < 0 {
		return "-" + cents(-c)
	}
	euros := c / 100
	frac := c % 100
	return fmt.Sprintf("%d.%02d", euros, frac)
}
