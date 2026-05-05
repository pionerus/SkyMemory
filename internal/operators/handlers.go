// Package operators implements the club-admin /admin/operators page —
// where the owner adds / lists / removes operator-role accounts inside
// their tenant. Adding a new operator here is what unlocks the
// /operator/* portal for that person; the owner is the only role that
// can do this so the package mounts behind sessions.RequireOwner.
//
// License tokens (machine credentials for studio.exe) live in a separate
// package (internal/auth) and a separate page (/admin/tokens). One
// operator can have many tokens (multiple machines).
package operators

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
)

// Handlers wires /admin/operators/* routes. Mounted behind RequireOwner.
type Handlers struct {
	DB        *db.Pool
	Templates Renderer
}

// Renderer matches html/template.ExecuteTemplate so the package doesn't
// import web/server/templates.
type Renderer interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}

// =====================================================================
// Data types
// =====================================================================

// Row is one operator row on the page. ClientCount = how many clients
// the club admin has assigned to this operator (helps the assign UI).
type Row struct {
	ID          int64
	Email       string
	Role        string
	CreatedAt   time.Time
	LastLoginAt *time.Time
	ClientCount int
	JumpCount   int
}

// HumanInitials picks 1-2 chars from the email's local-part for the avatar.
func (r Row) HumanInitials() string {
	at := strings.IndexByte(r.Email, '@')
	if at <= 0 {
		return "?"
	}
	local := r.Email[:at]
	local = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(local)
	parts := strings.Fields(strings.TrimSpace(local))
	out := []rune{}
	for _, p := range parts {
		ru := []rune(p)
		if len(ru) == 0 {
			continue
		}
		if ru[0] >= 'a' && ru[0] <= 'z' {
			ru[0] = ru[0] - 'a' + 'A'
		}
		out = append(out, ru[0])
		if len(out) >= 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// PageData is the payload for admin_operators.html.
type PageData struct {
	Active         string
	OperatorEmail  string
	OperatorRole   string
	TenantName     string
	TenantInitials string
	PlanLabel      string

	Operators       []Row
	OwnerCount      int
	OperatorCount   int
	UnassignedClubClients int
}

// =====================================================================
// GET /admin/operators
// =====================================================================
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.DB.Query(ctx, `
		SELECT
			o.id,
			o.email,
			o.role,
			o.created_at,
			o.last_login_at,
			COALESCE((SELECT COUNT(*) FROM clients c
			          WHERE c.assigned_operator_id = o.id), 0) AS client_n,
			COALESCE((SELECT COUNT(*) FROM jumps j
			          WHERE j.operator_id = o.id), 0)            AS jump_n
		FROM operators o
		WHERE o.tenant_id = $1
		ORDER BY o.role DESC, o.id ASC`, // owners first, then operators
		s.TenantID,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := make([]Row, 0, 8)
	var owners, operators int
	for rows.Next() {
		var row Row
		var lastLogin *time.Time
		if err := rows.Scan(
			&row.ID, &row.Email, &row.Role,
			&row.CreatedAt, &lastLogin,
			&row.ClientCount, &row.JumpCount,
		); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		row.LastLoginAt = lastLogin
		out = append(out, row)
		if row.Role == "owner" {
			owners++
		} else {
			operators++
		}
	}

	// Tenant chrome (matches adminPageData() in cmd/server).
	var tenantName string
	_ = h.DB.QueryRow(ctx,
		`SELECT name FROM tenants WHERE id = $1`, s.TenantID,
	).Scan(&tenantName)
	if tenantName == "" {
		tenantName = "Tenant"
	}

	var unassigned int
	_ = h.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM clients WHERE tenant_id = $1 AND assigned_operator_id IS NULL`,
		s.TenantID,
	).Scan(&unassigned)

	planLbl := "€0.00 this month"
	{
		y, m := billing.CurrentMonth()
		if b, berr := billing.Compute(ctx, h.DB, s.TenantID, y, m); berr == nil && b != nil {
			planLbl = "€" + b.EuroTotal() + " this month"
		}
	}

	data := PageData{
		Active:                "operators-people",
		OperatorEmail:         s.OperatorEmail,
		OperatorRole:          s.OperatorRole,
		TenantName:            tenantName,
		TenantInitials:        tenantInitials(tenantName),
		PlanLabel:             planLbl,
		Operators:             out,
		OwnerCount:            owners,
		OperatorCount:         operators,
		UnassignedClubClients: unassigned,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.Templates.ExecuteTemplate(w, "admin_operators.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// =====================================================================
// POST /admin/operators
// =====================================================================

// CreateRequest is the JSON body of the add-operator modal.
type CreateRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"` // 'operator' (default) or 'owner'
}

// CreateResponse is what the modal's JS expects.
type CreateResponse struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// Create adds a new operator-role (or extra owner) to the current tenant.
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())

	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Role = strings.TrimSpace(strings.ToLower(req.Role))
	if req.Role == "" {
		req.Role = "operator"
	}

	if req.Email == "" || !strings.Contains(req.Email, "@") {
		writeJSONErr(w, http.StatusBadRequest, "EMAIL", "Email is required.")
		return
	}
	if len(req.Password) < 8 {
		writeJSONErr(w, http.StatusBadRequest, "PASSWORD", "Password must be at least 8 characters.")
		return
	}
	if req.Role != "operator" && req.Role != "owner" {
		writeJSONErr(w, http.StatusBadRequest, "ROLE", "Role must be 'operator' or 'owner'.")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "HASH", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var id int64
	err = h.DB.QueryRow(ctx, `
		INSERT INTO operators (tenant_id, email, password_hash, role)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		s.TenantID, req.Email, hash, req.Role,
	).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "operators_tenant_id_email_key") {
			writeJSONErr(w, http.StatusConflict, "EMAIL_TAKEN",
				"An operator with this email already exists in this club.")
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, "INSERT", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(CreateResponse{
		ID: id, Email: req.Email, Role: req.Role,
	})
}

// =====================================================================
// DELETE /admin/operators/{id}
// =====================================================================
//
// Hard-deletes the row. Foreign keys: clients.assigned_operator_id has
// ON DELETE SET NULL (migration 0005), license_tokens.operator_id has
// ON DELETE CASCADE (migration 0001), so removing an operator detaches
// their clients and revokes their tokens automatically.
//
// Owners can NOT delete themselves — guard against locking the tenant
// out of admin access.
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	idParam := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idParam, 10, 64)
	if err != nil || id <= 0 {
		writeJSONErr(w, http.StatusBadRequest, "INVALID_ID", "id must be a positive integer.")
		return
	}
	if id == s.OperatorID {
		writeJSONErr(w, http.StatusBadRequest, "SELF",
			"You can't delete your own account from here. Sign out first or have another owner do it.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ct, err := h.DB.Exec(ctx,
		`DELETE FROM operators WHERE id = $1 AND tenant_id = $2`,
		id, s.TenantID,
	)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "DELETE", err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeJSONErr(w, http.StatusNotFound, "NOT_FOUND", "Operator not found in this club.")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// =====================================================================
// GET /admin/operators/json — used by the assign-operator dropdown on
// the clients page. Returns the same operators visible on the operators
// page but as JSON for client-side consumption.
// =====================================================================
func (h *Handlers) ListJSON(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.DB.Query(ctx,
		`SELECT id, email, role
		 FROM operators
		 WHERE tenant_id = $1
		 ORDER BY role DESC, email ASC`,
		s.TenantID,
	)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	defer rows.Close()
	type entry struct {
		ID    int64  `json:"id"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	var out []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.Email, &e.Role); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, e)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"operators": out})
}

// =====================================================================
// helpers
// =====================================================================

func tenantInitials(name string) string {
	out := []rune{}
	prevSep := true
	for _, r := range name {
		if r == ' ' || r == '-' || r == '_' {
			prevSep = true
			continue
		}
		if prevSep && len(out) < 2 {
			if r >= 'a' && r <= 'z' {
				r = r - 'a' + 'A'
			}
			out = append(out, r)
		}
		prevSep = false
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

func writeJSONErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

// reserve imports for future endpoints
var (
	_ = errors.Is
	_ = fmt.Errorf
	_ = pgx.ErrNoRows
)
