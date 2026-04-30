// Package platform implements the cross-tenant super-admin endpoints
// (/platform/*). Phase 10.2 — clubs CRUD with per-club aggregations.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/pionerus/freefall/internal/auth"
	"github.com/pionerus/freefall/internal/db"
)

// Handlers wires the /platform/clubs/* HTML + JSON endpoints.
type Handlers struct {
	DB        *db.Pool
	Templates Renderer
	BaseURL   string // e.g. https://skydivememory.app — for slug links on E2
}

// Renderer matches html/template's ExecuteTemplate so we don't import
// web/server/templates from this package.
type Renderer interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}

// =====================================================================
// E1 — Clubs list
// =====================================================================

// ClubRow is one row in /platform/clubs. Aggregations are computed in a
// single query with LEFT JOINs onto count subqueries — the platform-admin
// dashboard isn't paginated yet so we accept O(N tenants) once per page load.
type ClubRow struct {
	ID          int64
	Name        string
	Slug        string
	CountryCode string
	City        string
	Plan        string
	Status      string
	OperatorN   int
	JumpN       int
	PhotoOrderN int
	JoinedAt    time.Time
}

// Country returns "Yekaterinburg, RU" for the table's location column.
func (c ClubRow) Country() string {
	parts := []string{}
	if c.City != "" {
		parts = append(parts, c.City)
	}
	if c.CountryCode != "" {
		parts = append(parts, c.CountryCode)
	}
	return strings.Join(parts, ", ")
}

// Flag returns the regional-indicator emoji for a 2-letter ISO code (e.g. "RU"
// → 🇷🇺). Renders as plain text in browsers that don't support flag emoji,
// which is fine.
func (c ClubRow) Flag() string {
	if len(c.CountryCode) != 2 {
		return ""
	}
	cc := strings.ToUpper(c.CountryCode)
	out := []rune{}
	for _, r := range cc {
		if r < 'A' || r > 'Z' {
			return ""
		}
		out = append(out, 0x1F1E6+(r-'A'))
	}
	return string(out)
}

// ListClubs returns every active tenant + its aggregations. Soft-deleted
// tenants (deleted_at IS NOT NULL) are excluded.
func (h *Handlers) listClubs(ctx context.Context) ([]ClubRow, error) {
	rows, err := h.DB.Query(ctx, `
		SELECT
			t.id,
			t.name,
			COALESCE(t.slug, ''),
			COALESCE(t.country_code, ''),
			COALESCE(t.city, ''),
			t.plan,
			t.status,
			COALESCE((SELECT COUNT(*) FROM operators o WHERE o.tenant_id = t.id), 0) AS op_n,
			COALESCE((SELECT COUNT(*) FROM jumps j    WHERE j.tenant_id = t.id), 0) AS jump_n,
			COALESCE((SELECT COUNT(*) FROM photo_orders po
			          JOIN jumps jj ON jj.id = po.jump_id
			          WHERE jj.tenant_id = t.id), 0) AS photo_n,
			t.created_at
		FROM tenants t
		WHERE t.deleted_at IS NULL
		ORDER BY t.created_at DESC, t.id DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list clubs: %w", err)
	}
	defer rows.Close()

	var out []ClubRow
	for rows.Next() {
		var c ClubRow
		if err := rows.Scan(
			&c.ID, &c.Name, &c.Slug, &c.CountryCode, &c.City, &c.Plan, &c.Status,
			&c.OperatorN, &c.JumpN, &c.PhotoOrderN, &c.JoinedAt,
		); err != nil {
			return nil, fmt.Errorf("scan club row: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClubsListPageData is the template payload for E1.
type ClubsListPageData struct {
	AdminName    string
	Clubs        []ClubRow
	TotalClubs   int
	ActiveCount  int
	TrialCount   int
	OverdueCount int
	TotalOps     int
	TotalJumps   int
	TotalPhotos  int
}

// ClubsList handles GET /platform/clubs.
func (h *Handlers) ClubsList(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	clubs, err := h.listClubs(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := ClubsListPageData{
		AdminName: s.PlatformAdminName,
		Clubs:     clubs,
	}
	for _, c := range clubs {
		data.TotalClubs++
		switch c.Status {
		case "active":
			data.ActiveCount++
		case "trial":
			data.TrialCount++
		case "overdue":
			data.OverdueCount++
		}
		data.TotalOps += c.OperatorN
		data.TotalJumps += c.JumpN
		data.TotalPhotos += c.PhotoOrderN
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.Templates.ExecuteTemplate(w, "platform_clubs.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// =====================================================================
// E2 — Club detail
// =====================================================================

// ClubDetail aggregates the data the detail page renders.
type ClubDetail struct {
	Club ClubRow

	OwnerName   string
	OwnerEmail  string
	OwnerJoined time.Time

	// Series of (videos rendered) per month for the last 12 months,
	// oldest → newest. Indices align with MonthLabels.
	Series      []int
	MonthLabels []string
	MaxSeries   int
	BestMonth   string
	AvgPerMonth int

	RecentJumps []RecentJump
}

// RecentJump is one row of the "Recent jumps" panel.
type RecentJump struct {
	AccessCode string
	ClientName string
	OperatorEm string
	When       time.Time
	Status     string
}

// ClubDetail handles GET /platform/clubs/{id}.
func (h *Handlers) ClubDetail(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	idParam := chi.URLParam(r, "id")
	id, err := parseInt64(idParam)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Pull the row + owner contact + activity series in three sequential
	// queries. Could be one query with CTEs but the readability hit isn't
	// worth the latency saving for an admin page.
	var club ClubRow
	err = h.DB.QueryRow(ctx, `
		SELECT
			t.id, t.name, COALESCE(t.slug, ''),
			COALESCE(t.country_code, ''), COALESCE(t.city, ''),
			t.plan, t.status,
			COALESCE((SELECT COUNT(*) FROM operators o WHERE o.tenant_id = t.id), 0),
			COALESCE((SELECT COUNT(*) FROM jumps j    WHERE j.tenant_id = t.id), 0),
			COALESCE((SELECT COUNT(*) FROM photo_orders po
			          JOIN jumps jj ON jj.id = po.jump_id
			          WHERE jj.tenant_id = t.id), 0),
			t.created_at
		FROM tenants t
		WHERE t.id = $1 AND t.deleted_at IS NULL`,
		id,
	).Scan(
		&club.ID, &club.Name, &club.Slug, &club.CountryCode, &club.City,
		&club.Plan, &club.Status,
		&club.OperatorN, &club.JumpN, &club.PhotoOrderN, &club.JoinedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Owner = first operator with role='owner', oldest. (Single-owner clubs
	// is the common case; future multi-owner support won't break this.)
	detail := ClubDetail{Club: club}
	_ = h.DB.QueryRow(ctx, `
		SELECT email, COALESCE(last_login_at, created_at), created_at
		FROM operators
		WHERE tenant_id = $1 AND role = 'owner'
		ORDER BY created_at ASC
		LIMIT 1`, id,
	).Scan(&detail.OwnerEmail, new(time.Time), &detail.OwnerJoined)
	detail.OwnerName = displayNameFromEmail(detail.OwnerEmail)

	// Activity series: jumps per month for the last 12 months (oldest → newest).
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -11, 0)
	rows, err := h.DB.Query(ctx, `
		SELECT
		  EXTRACT(YEAR FROM date_trunc('month', created_at))::int  AS y,
		  EXTRACT(MONTH FROM date_trunc('month', created_at))::int AS m,
		  COUNT(*) AS n
		FROM jumps
		WHERE tenant_id = $1 AND created_at >= $2
		GROUP BY 1, 2
		ORDER BY 1, 2`,
		id, first,
	)
	if err == nil {
		bucket := map[string]int{}
		defer rows.Close()
		for rows.Next() {
			var y, m, n int
			if err := rows.Scan(&y, &m, &n); err == nil {
				bucket[fmt.Sprintf("%04d-%02d", y, m)] = n
			}
		}
		// Walk 12 months and fill zeros where no jumps happened.
		for i := 0; i < 12; i++ {
			t := first.AddDate(0, i, 0)
			key := t.Format("2006-01")
			n := bucket[key]
			detail.Series = append(detail.Series, n)
			detail.MonthLabels = append(detail.MonthLabels, t.Format("Jan"))
			if n > detail.MaxSeries {
				detail.MaxSeries = n
				detail.BestMonth = t.Format("Jan")
			}
		}
		sum := 0
		for _, v := range detail.Series {
			sum += v
		}
		if len(detail.Series) > 0 {
			detail.AvgPerMonth = sum / len(detail.Series)
		}
	}

	// Last 5 jumps for the bottom panel.
	recRows, err := h.DB.Query(ctx, `
		SELECT c.access_code, c.name, COALESCE(o.email, ''), j.created_at, j.status
		FROM jumps j
		JOIN clients c ON c.id = j.client_id
		LEFT JOIN operators o ON o.id = j.operator_id
		WHERE j.tenant_id = $1
		ORDER BY j.created_at DESC
		LIMIT 5`, id)
	if err == nil {
		defer recRows.Close()
		for recRows.Next() {
			var rj RecentJump
			if err := recRows.Scan(&rj.AccessCode, &rj.ClientName, &rj.OperatorEm, &rj.When, &rj.Status); err == nil {
				detail.RecentJumps = append(detail.RecentJumps, rj)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s // AdminName is rendered through the rail partial elsewhere; the
	      // detail page renders its own topbar, so we don't pass admin name.
	if err := h.Templates.ExecuteTemplate(w, "platform_club_detail.html", detail); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// =====================================================================
// E3 — Create club (POST /platform/clubs)
// =====================================================================

// CreateClubRequest is the body of the create-club form.
type CreateClubRequest struct {
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	City          string `json:"city"`
	CountryCode   string `json:"country_code"`
	Plan          string `json:"plan"`
	OwnerName     string `json:"owner_name"`
	OwnerEmail    string `json:"owner_email"`
	OwnerPassword string `json:"owner_password"`
}

// CreateClubResponse is what the form's JS expects on success.
type CreateClubResponse struct {
	TenantID    int64  `json:"tenant_id"`
	OperatorID  int64  `json:"operator_id"`
	OwnerEmail  string `json:"owner_email"`
	RedirectURL string `json:"redirect_url"`
}

// CreateClub handles POST /platform/clubs (JSON body). Creates a tenant
// row + an owner operator inside a single transaction. The temporary
// password is not echoed back — the platform admin entered it.
func (h *Handlers) CreateClub(w http.ResponseWriter, r *http.Request) {
	var req CreateClubRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(strings.ToLower(req.Slug))
	req.OwnerEmail = strings.TrimSpace(strings.ToLower(req.OwnerEmail))
	req.OwnerName = strings.TrimSpace(req.OwnerName)
	req.City = strings.TrimSpace(req.City)
	req.CountryCode = strings.TrimSpace(strings.ToUpper(req.CountryCode))

	if req.Name == "" || len(req.Name) > 200 {
		writeJSONErr(w, http.StatusBadRequest, "NAME", "Club name is required (≤200 chars).")
		return
	}
	if req.Slug == "" || !validSlug(req.Slug) {
		writeJSONErr(w, http.StatusBadRequest, "SLUG", "Slug must be 3..40 lowercase letters/digits/dashes.")
		return
	}
	if req.OwnerEmail == "" || !strings.Contains(req.OwnerEmail, "@") {
		writeJSONErr(w, http.StatusBadRequest, "EMAIL", "Owner email is required.")
		return
	}
	if len(req.OwnerPassword) < 8 {
		writeJSONErr(w, http.StatusBadRequest, "PASSWORD", "Owner password must be at least 8 characters.")
		return
	}
	if req.CountryCode != "" && len(req.CountryCode) != 2 {
		writeJSONErr(w, http.StatusBadRequest, "COUNTRY", "Country code must be a 2-letter ISO code.")
		return
	}
	plan := strings.ToLower(req.Plan)
	if plan == "" {
		plan = "starter"
	}
	switch plan {
	case "starter", "pro", "enterprise":
	default:
		writeJSONErr(w, http.StatusBadRequest, "PLAN", "Plan must be one of: starter, pro, enterprise.")
		return
	}

	hash, herr := auth.HashPassword(req.OwnerPassword)
	if herr != nil {
		writeJSONErr(w, http.StatusInternalServerError, "HASH", herr.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "DB_BEGIN", err.Error())
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tenantID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO tenants (name, slug, plan, status, country_code, city)
		VALUES ($1, $2, $3, 'active', NULLIF($4,''), NULLIF($5,''))
		RETURNING id`,
		req.Name, req.Slug, plan, req.CountryCode, req.City,
	).Scan(&tenantID)
	if err != nil {
		// Cleanest UX surface for a slug collision: explicit 409.
		if strings.Contains(err.Error(), "tenants_slug_key") || strings.Contains(err.Error(), "duplicate") {
			writeJSONErr(w, http.StatusConflict, "SLUG_TAKEN", "Slug already in use.")
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, "INSERT_TENANT", err.Error())
		return
	}

	var operatorID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO operators (tenant_id, email, password_hash, role)
		VALUES ($1, $2, $3, 'owner')
		RETURNING id`,
		tenantID, req.OwnerEmail, hash,
	).Scan(&operatorID)
	if err != nil {
		if strings.Contains(err.Error(), "operators_tenant_id_email_key") {
			writeJSONErr(w, http.StatusConflict, "EMAIL_TAKEN", "Owner email already exists in this club.")
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, "INSERT_OWNER", err.Error())
		return
	}

	if err := tx.Commit(ctx); err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "COMMIT", err.Error())
		return
	}

	redirect := fmt.Sprintf("/platform/clubs/%d", tenantID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(CreateClubResponse{
		TenantID:    tenantID,
		OperatorID:  operatorID,
		OwnerEmail:  req.OwnerEmail,
		RedirectURL: redirect,
	})
}

// =====================================================================
// helpers
// =====================================================================

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil || n <= 0 {
		return 0, errors.New("invalid id")
	}
	return n, nil
}

// validSlug — 3..40 chars of [a-z0-9-], not starting/ending with a dash.
func validSlug(s string) bool {
	if len(s) < 3 || len(s) > 40 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// displayNameFromEmail turns "dmitri.k@aeroclub-ural.ru" → "Dmitri K".
// Falls back to "Owner" if the local-part is empty / weird.
func displayNameFromEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "Owner"
	}
	local := email[:at]
	local = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(local)
	parts := strings.Fields(strings.TrimSpace(local))
	for i, p := range parts {
		if len(p) == 0 {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	if len(parts) == 0 {
		return "Owner"
	}
	return strings.Join(parts, " ")
}

func writeJSONErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}
