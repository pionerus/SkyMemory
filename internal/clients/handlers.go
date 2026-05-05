// Package clients implements the club-admin /admin/clients page (B6) —
// the per-tenant jumper roster with their latest jump status + assigned
// operator. Mounted under owner-scoped routes.
package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/pionerus/freefall/internal/auth"
	"github.com/pionerus/freefall/internal/billing"
	"github.com/pionerus/freefall/internal/db"
	"github.com/pionerus/freefall/internal/jump"
)

// Handlers wires /admin/clients/* HTML + JSON endpoints. Routes are mounted
// behind sessions.RequireOwner so SessionData has the tenant + role we need.
type Handlers struct {
	DB        *db.Pool
	Templates Renderer
}

// Renderer matches html/template.Template.ExecuteTemplate so this package
// stays decoupled from web/server/templates.
type Renderer interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}

// =====================================================================
// GET /admin/clients
// =====================================================================

// ClientRow is one row of the clients table. LatestJumpAt is zero when the
// jumper has been added but no jump has been booked yet.
type ClientRow struct {
	ID             int64
	Name           string
	Email          string
	Phone          string
	AccessCode     string
	CreatedAt      time.Time
	PlannedJumpAt  time.Time // scheduled jump date, zero if unset
	LatestJumpAt   time.Time
	LatestJumpID   int64
	LatestStatus   string

	// Assigned-operator state. The "preferred" operator that the club
	// admin picks when adding the client. Distinct from the operator
	// who actually filmed any historical jump (`LatestJumpOperator`).
	AssignedOperatorID    int64  // 0 means unassigned
	AssignedOperatorEmail string // joined for display

	// Latest-jump operator — set on jumps.operator_id when the studio
	// register flow runs. Shown next to the assignment so the admin can
	// see drift ("assigned to A but B actually filmed").
	LatestJumpOperator string

	OperatorInits  string // initials of AssignedOperatorEmail for the avatar
	JumpCount      int
}

// PageData is the payload for admin_clients.html.
type PageData struct {
	Active         string // for the rail's is-active marker
	OperatorEmail  string
	OperatorRole   string
	TenantName     string
	TenantInitials string
	PlanLabel      string

	Clients          []ClientRow
	TotalClients     int
	UnassignedCount  int
	UpcomingCount    int // clients whose latest jump is still in 'draft' / 'editing' / 'encoding'
}

// Initials lifts the first letter of each name word into a 1-2 char string
// for the row's avatar tile. "Anna Vorobyeva" → "AV". Falls back to "?".
func (c ClientRow) Initials() string {
	if c.Name == "" {
		return "?"
	}
	parts := strings.Fields(c.Name)
	out := []rune{}
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		r := []rune(p)[0]
		if r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		out = append(out, r)
		if len(out) >= 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// DashedAccessCode returns the canonical 8-char code as "XXXX-XXXX" for
// display in tables. Stored without dash in the DB.
func (c ClientRow) DashedAccessCode() string {
	if len(c.AccessCode) == 8 {
		return c.AccessCode[:4] + "-" + c.AccessCode[4:]
	}
	return c.AccessCode
}

// List handles GET /admin/clients.
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.DB.Query(ctx, `
		SELECT
			v.client_id, v.name, COALESCE(v.email, ''), COALESCE(v.phone, ''),
			v.access_code, v.client_created_at,
			COALESCE(v.planned_jump_at, '0001-01-01'::timestamptz),
			COALESCE(v.jump_created_at, '0001-01-01'::timestamptz),
			COALESCE(v.jump_id, 0),
			v.status,
			COALESCE(v.assigned_operator_id, 0),
			COALESCE(assigned.email, ''),
			COALESCE(latestop.email, ''),
			COALESCE((SELECT COUNT(*) FROM jumps jj WHERE jj.client_id = v.client_id), 0)
		FROM v_client_status v
		LEFT JOIN operators assigned  ON assigned.id  = v.assigned_operator_id
		LEFT JOIN jumps latest_j      ON latest_j.id  = v.jump_id
		LEFT JOIN operators latestop  ON latestop.id  = latest_j.operator_id
		WHERE v.tenant_id = $1
		ORDER BY COALESCE(v.planned_jump_at, v.jump_created_at, v.client_created_at) DESC
		LIMIT 500`,
		s.TenantID,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	clients := make([]ClientRow, 0, 32)
	for rows.Next() {
		var c ClientRow
		if err := rows.Scan(
			&c.ID, &c.Name, &c.Email, &c.Phone,
			&c.AccessCode, &c.CreatedAt,
			&c.PlannedJumpAt,
			&c.LatestJumpAt, &c.LatestJumpID, &c.LatestStatus,
			&c.AssignedOperatorID, &c.AssignedOperatorEmail,
			&c.LatestJumpOperator, &c.JumpCount,
		); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.OperatorInits = operatorInitials(c.AssignedOperatorEmail)
		clients = append(clients, c)
	}

	// Tenant chrome (same shape as adminPageData() in cmd/server).
	var tenantName string
	_ = h.DB.QueryRow(ctx,
		`SELECT name FROM tenants WHERE id = $1`, s.TenantID,
	).Scan(&tenantName)
	if tenantName == "" {
		tenantName = "Tenant"
	}

	// Rail's "PlanLabel" slot now shows current month's bill total.
	planLbl := "€0.00 this month"
	{
		y, m := billing.CurrentMonth()
		if b, berr := billing.Compute(ctx, h.DB, s.TenantID, y, m); berr == nil && b != nil {
			planLbl = "€" + b.EuroTotal() + " this month"
		}
	}

	data := PageData{
		Active:         "clients",
		OperatorEmail:  s.OperatorEmail,
		OperatorRole:   s.OperatorRole,
		TenantName:     tenantName,
		TenantInitials: tenantInitials(tenantName),
		PlanLabel:      planLbl,
		Clients:        clients,
	}
	for _, c := range clients {
		data.TotalClients++
		// "Unassigned" = no operator picked yet. The latest-jump operator
		// is informational only; the assignment is what determines whose
		// /operator/clients view this client appears in.
		if c.AssignedOperatorID == 0 {
			data.UnassignedCount++
		}
		// "Upcoming" = client's project is somewhere mid-flight: assigned
		// to an operator OR jump is being shot/edited but no email out yet.
		switch c.LatestStatus {
		case "assigned", "in_progress":
			data.UpcomingCount++
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.Templates.ExecuteTemplate(w, "admin_clients.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// =====================================================================
// POST /admin/clients   (JSON body)
// =====================================================================

// CreateRequest is the body of the "Add client" modal form. AssignedOperatorID
// is optional — pass 0 (or omit) to leave the client unassigned. PlannedJumpAt
// is the scheduled jump date (YYYY-MM-DD or RFC3339); empty leaves it NULL.
type CreateRequest struct {
	Name               string `json:"name"`
	Email              string `json:"email"`
	Phone              string `json:"phone"`
	AssignedOperatorID int64  `json:"assigned_operator_id"`
	PlannedJumpAt      string `json:"planned_jump_at"`
}

// CreateResponse is the success payload returned to the modal's JS.
type CreateResponse struct {
	ID         int64  `json:"id"`
	AccessCode string `json:"access_code"`
}

// Create handles POST /admin/clients. Generates a fresh access_code (same
// alphabet the studio register flow uses — Crockford-Base32 minus I/L/O/U).
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Phone = strings.TrimSpace(req.Phone)
	if req.Name == "" || len(req.Name) > 200 {
		writeJSONErr(w, http.StatusBadRequest, "NAME", "Client name is required (≤200 chars).")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// If an operator was picked, validate it belongs to the same tenant.
	// Defends against a malicious form submission with a foreign id.
	if req.AssignedOperatorID > 0 {
		var ok bool
		err := h.DB.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM operators WHERE id = $1 AND tenant_id = $2)`,
			req.AssignedOperatorID, s.TenantID,
		).Scan(&ok)
		if err != nil || !ok {
			writeJSONErr(w, http.StatusBadRequest, "OPERATOR",
				"Selected operator doesn't belong to this club.")
			return
		}
	}

	plannedAt, perr := parseJumpDate(req.PlannedJumpAt)
	if perr != nil {
		writeJSONErr(w, http.StatusBadRequest, "DATE", perr.Error())
		return
	}

	// Insert with retry on access_code collision (we generate; the unique
	// index would reject on a clash, which is astronomically unlikely but
	// handle it cleanly anyway).
	var (
		id   int64
		code string
	)
	for attempt := 0; attempt < 5; attempt++ {
		var err error
		code, _, err = jump.NewAccessCode()
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "RAND", err.Error())
			return
		}
		err = h.DB.QueryRow(ctx, `
			INSERT INTO clients (tenant_id, name, email, phone, access_code, created_by, assigned_operator_id, planned_jump_at)
			VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5, $6, NULLIF($7,0)::bigint, $8)
			RETURNING id`,
			s.TenantID, req.Name, req.Email, req.Phone, code, s.OperatorID, req.AssignedOperatorID, plannedAt,
		).Scan(&id)
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "clients_access_code_key") {
			continue // try again with a fresh code
		}
		writeJSONErr(w, http.StatusInternalServerError, "INSERT", err.Error())
		return
	}
	if id == 0 {
		writeJSONErr(w, http.StatusInternalServerError, "RAND_RETRIES", "could not generate a unique access code")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(CreateResponse{ID: id, AccessCode: code})
}

// =====================================================================
// PUT /admin/clients/{id}/assign  (JSON body: {operator_id: <int64>})
// =====================================================================
//
// Reassigns a client to a different operator (or detaches via 0). Only
// operators within the same tenant are accepted.
func (h *Handlers) Assign(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	cid := chi.URLParam(r, "id")
	clientID, err := strconv.ParseInt(cid, 10, 64)
	if err != nil || clientID <= 0 {
		writeJSONErr(w, http.StatusBadRequest, "INVALID_ID", "client id required")
		return
	}

	var req struct {
		OperatorID int64 `json:"operator_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if req.OperatorID > 0 {
		var ok bool
		_ = h.DB.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM operators WHERE id = $1 AND tenant_id = $2)`,
			req.OperatorID, s.TenantID,
		).Scan(&ok)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "OPERATOR", "operator not in this club")
			return
		}
	}

	ct, err := h.DB.Exec(ctx,
		`UPDATE clients SET assigned_operator_id = NULLIF($1,0)::bigint
		 WHERE id = $2 AND tenant_id = $3`,
		req.OperatorID, clientID, s.TenantID,
	)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeJSONErr(w, http.StatusNotFound, "NOT_FOUND", "client not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"client_id":   clientID,
		"operator_id": req.OperatorID,
	})
}

// =====================================================================
// helpers
// =====================================================================

func tenantInitials(name string) string {
	out := []rune{}
	prevWasSep := true
	for _, r := range name {
		if r == ' ' || r == '-' || r == '_' {
			prevWasSep = true
			continue
		}
		if prevWasSep && len(out) < 2 {
			if r >= 'a' && r <= 'z' {
				r = r - 'a' + 'A'
			}
			out = append(out, r)
		}
		prevWasSep = false
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

func operatorInitials(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return ""
	}
	local := email[:at]
	local = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(local)
	parts := strings.Fields(strings.TrimSpace(local))
	out := []rune{}
	for _, p := range parts {
		r := []rune(p)[0]
		if r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		out = append(out, r)
		if len(out) >= 2 {
			break
		}
	}
	return string(out)
}

func writeJSONErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

// =====================================================================
// PUT /admin/clients/{id}   (JSON body — full edit)
// =====================================================================

// UpdateRequest covers every editable field on the row. Empty Name keeps
// the existing value (treated as "no change") so the front-end can send
// only the fields it actually changed.
type UpdateRequest struct {
	Name               string `json:"name"`
	Email              string `json:"email"`
	Phone              string `json:"phone"`
	AssignedOperatorID *int64 `json:"assigned_operator_id"` // pointer = optional, 0 = unassign
	PlannedJumpAt      string `json:"planned_jump_at"`      // empty string = clear; "skip" = leave unchanged
	ClearPlannedJumpAt bool   `json:"clear_planned_jump_at"`
}

// Update handles PUT /admin/clients/{id}. All fields are optional; missing
// or empty fields preserve the existing value (except for explicit clearing
// flags). Tenant-scoped — won't touch a row that belongs to another club.
func (h *Handlers) Update(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	clientID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || clientID <= 0 {
		writeJSONErr(w, http.StatusBadRequest, "INVALID_ID", "client id required")
		return
	}
	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Phone = strings.TrimSpace(req.Phone)
	if req.Name == "" {
		writeJSONErr(w, http.StatusBadRequest, "NAME", "Name is required.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Validate operator belongs to this tenant when assignment is supplied.
	if req.AssignedOperatorID != nil && *req.AssignedOperatorID > 0 {
		var ok bool
		_ = h.DB.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM operators WHERE id = $1 AND tenant_id = $2)`,
			*req.AssignedOperatorID, s.TenantID,
		).Scan(&ok)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "OPERATOR", "operator not in this club")
			return
		}
	}

	var plannedAt any // *time.Time | "leave unchanged" sentinel via map column
	switch {
	case req.ClearPlannedJumpAt:
		plannedAt = nil
	case req.PlannedJumpAt != "":
		t, perr := parseJumpDate(req.PlannedJumpAt)
		if perr != nil {
			writeJSONErr(w, http.StatusBadRequest, "DATE", perr.Error())
			return
		}
		plannedAt = t
	default:
		plannedAt = "__SKIP__" // sentinel — handled below
	}

	q := `
		UPDATE clients SET
			name  = $1,
			email = NULLIF($2, ''),
			phone = NULLIF($3, '')`
	args := []any{req.Name, req.Email, req.Phone}
	idx := 4
	if req.AssignedOperatorID != nil {
		q += fmt.Sprintf(", assigned_operator_id = NULLIF($%d, 0)::bigint", idx)
		args = append(args, *req.AssignedOperatorID)
		idx++
	}
	if pa, ok := plannedAt.(string); !ok || pa != "__SKIP__" {
		q += fmt.Sprintf(", planned_jump_at = $%d", idx)
		args = append(args, plannedAt)
		idx++
	}
	q += fmt.Sprintf(" WHERE id = $%d AND tenant_id = $%d", idx, idx+1)
	args = append(args, clientID, s.TenantID)

	ct, err := h.DB.Exec(ctx, q, args...)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeJSONErr(w, http.StatusNotFound, "NOT_FOUND", "client not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": clientID, "ok": true})
}

// =====================================================================
// DELETE /admin/clients/{id}
// =====================================================================
//
// Permanent delete. Cascades to jumps via FK ON DELETE CASCADE, which in
// turn cascades to jump_artifacts / jump_terms / watch_events. Refuses if
// the operator hasn't passed `?confirm=1` so a stray double-click on the
// row doesn't nuke a real jumper.
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	clientID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || clientID <= 0 {
		writeJSONErr(w, http.StatusBadRequest, "INVALID_ID", "client id required")
		return
	}
	if r.URL.Query().Get("confirm") != "1" {
		writeJSONErr(w, http.StatusPreconditionRequired, "CONFIRM", "Add ?confirm=1 to confirm the delete.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	ct, err := h.DB.Exec(ctx,
		`DELETE FROM clients WHERE id = $1 AND tenant_id = $2`,
		clientID, s.TenantID,
	)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeJSONErr(w, http.StatusNotFound, "NOT_FOUND", "client not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": clientID, "deleted": true})
}

// =====================================================================
// POST /admin/clients/import   (text/csv body)
// =====================================================================
//
// Accepts plain CSV (or pasted lines) with columns: name, email, phone,
// planned_jump_at, operator_email. Header row is optional. Blank lines and
// `#` comments are ignored. Per-row failures are collected and returned in
// the response — partial success is OK ("rows 5+8 had no name"). The
// front-end shows the failure list in a banner.
func (h *Handlers) ImportCSV(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10)) // 256 KB cap
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "READ", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Snapshot tenant operators for email→id lookup.
	opByEmail := map[string]int64{}
	{
		rows, _ := h.DB.Query(ctx, `SELECT id, lower(email) FROM operators WHERE tenant_id = $1`, s.TenantID)
		for rows.Next() {
			var id int64
			var email string
			_ = rows.Scan(&id, &email)
			opByEmail[email] = id
		}
		rows.Close()
	}

	type rowResult struct {
		Line       int    `json:"line"`
		Name       string `json:"name,omitempty"`
		AccessCode string `json:"access_code,omitempty"`
		Error      string `json:"error,omitempty"`
	}
	var (
		results []rowResult
		ok      int
	)
	lines := strings.Split(string(body), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Skip an obvious header row on line 1.
		if i == 0 && strings.Contains(strings.ToLower(line), "name") &&
			(strings.Contains(strings.ToLower(line), "email") || strings.Contains(strings.ToLower(line), "phone")) {
			continue
		}
		cols := splitCSVLine(line)
		// columns: name, email, phone, planned_jump_at, operator_email
		name := strings.TrimSpace(getCol(cols, 0))
		email := strings.TrimSpace(strings.ToLower(getCol(cols, 1)))
		phone := strings.TrimSpace(getCol(cols, 2))
		plannedRaw := strings.TrimSpace(getCol(cols, 3))
		opEmail := strings.TrimSpace(strings.ToLower(getCol(cols, 4)))

		res := rowResult{Line: i + 1, Name: name}
		if name == "" {
			res.Error = "name is required"
			results = append(results, res)
			continue
		}
		var plannedAt any
		if plannedRaw != "" {
			t, err := parseJumpDate(plannedRaw)
			if err != nil {
				res.Error = "bad date: " + err.Error()
				results = append(results, res)
				continue
			}
			plannedAt = t
		}
		var opID int64
		if opEmail != "" {
			id, found := opByEmail[opEmail]
			if !found {
				res.Error = "operator " + opEmail + " not found in this club"
				results = append(results, res)
				continue
			}
			opID = id
		}

		// Insert with access-code retry, identical to Create().
		var (
			id   int64
			code string
		)
		var ierr error
		for attempt := 0; attempt < 5; attempt++ {
			code, _, ierr = jump.NewAccessCode()
			if ierr != nil {
				break
			}
			ierr = h.DB.QueryRow(ctx, `
				INSERT INTO clients (tenant_id, name, email, phone, access_code, created_by, assigned_operator_id, planned_jump_at)
				VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5, $6, NULLIF($7,0)::bigint, $8)
				RETURNING id`,
				s.TenantID, name, email, phone, code, s.OperatorID, opID, plannedAt,
			).Scan(&id)
			if ierr == nil || !strings.Contains(ierr.Error(), "clients_access_code_key") {
				break
			}
		}
		if ierr != nil {
			res.Error = ierr.Error()
		} else {
			res.AccessCode = code
			ok++
		}
		results = append(results, res)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"imported": ok,
		"results":  results,
	})
}

// parseJumpDate accepts YYYY-MM-DD (treated as midnight UTC), RFC3339, or
// "datetime-local" form (YYYY-MM-DDTHH:MM). Empty input → nil time.
func parseJumpDate(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{
		"2006-01-02",
		"2006-01-02T15:04",
		"2006-01-02T15:04:05",
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("could not parse %q (use YYYY-MM-DD or YYYY-MM-DDTHH:MM)", s)
}

// splitCSVLine splits a line on commas, treating "..." quoted segments as
// atomic so a name like "Doe, Jr." doesn't break the column count. Minimal —
// no full RFC-4180 escapes; club admins paste from Google Sheets and that's
// good enough.
func splitCSVLine(line string) []string {
	out := []string{}
	var cur strings.Builder
	inQuotes := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	out = append(out, cur.String())
	return out
}

func getCol(cols []string, i int) string {
	if i < 0 || i >= len(cols) {
		return ""
	}
	return cols[i]
}

// reserve `errors` + `fmt` + `pgx` imports in case future endpoints need them.
var _ = errors.Is
var _ = fmt.Errorf
var _ = pgx.ErrNoRows
