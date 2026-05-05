package jump

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/pionerus/freefall/internal/auth"
	"github.com/pionerus/freefall/internal/db"
	"github.com/pionerus/freefall/internal/email"
)

// EmailHandlers wires the deliverables-email endpoints. Constructed once
// at boot in cmd/server/main.go and shared across operator, admin, and
// studio-API routes.
type EmailHandlers struct {
	DB        *db.Pool
	Sender    *email.Sender
	Templates email.Renderer
	BaseURL   string // PublicBaseURL from config; used to build /watch/<code> URLs
}

// SendRequest is the body of POST /api/v1/jumps/{id}/send-email.
type SendRequest struct {
	Force bool `json:"force"`
}

// SendResponse is the body of every email-send endpoint.
type SendResponse struct {
	Sent      bool      `json:"sent"`
	Reason    string    `json:"reason"`
	Recipient string    `json:"recipient,omitempty"`
	SentAt    time.Time `json:"sent_at,omitempty"`
}

// Send handles POST /api/v1/jumps/{id}/send-email — invoked by studio
// after a render finishes uploading all artifacts. Idempotent unless
// force=true.
func (h *EmailHandlers) Send(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	jumpID, err := parseInt64Param(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}

	var req SendRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // empty body == {force:false}

	h.send(w, r.Context(), jumpID, s.TenantID, req.Force)
}

// Resend is the operator/admin manual variant — always force=true. Mounted
// behind RequireOwner / RequireSession depending on the route.
func (h *EmailHandlers) Resend(w http.ResponseWriter, r *http.Request) {
	s := auth.MustFromContext(r.Context())
	jumpID, err := parseInt64Param(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
		return
	}
	h.send(w, r.Context(), jumpID, s.TenantID, true)
}

func (h *EmailHandlers) send(w http.ResponseWriter, ctx context.Context, jumpID, tenantID int64, force bool) {
	if h.Sender == nil || !h.Sender.Configured() {
		writeError(w, http.StatusServiceUnavailable, "SMTP_NOT_CONFIGURED",
			"Set SMTP_HOST in the cloud .env (or run docker compose up -d mailhog for local dev).")
		return
	}

	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := email.SendDeliverables(subCtx, h.DB, h.Sender, h.Templates, email.SendDeliverablesParams{
		JumpID:   jumpID,
		TenantID: tenantID,
		BaseURL:  h.BaseURL,
		Force:    force,
	})
	if errors.Is(err, email.ErrNoEmail) {
		writeJSON(w, http.StatusPreconditionFailed, SendResponse{
			Sent:   false,
			Reason: email.ReasonNoEmail,
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "SEND_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, SendResponse{
		Sent:      res.Sent,
		Reason:    res.Reason,
		Recipient: res.Recipient,
		SentAt:    res.SentAt,
	})
}
