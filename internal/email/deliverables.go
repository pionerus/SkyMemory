package email

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pionerus/freefall/internal/db"
)

// Renderer is the interface served by web/server/templates.Templates
// (a *html/template.Template). Defined locally so this package stays a leaf
// — no circular import on web/server/templates.
type Renderer interface {
	ExecuteTemplate(w io.Writer, name string, data any) error
}

// Reasons returned by SendDeliverables. Maps onto the JSON response of the
// HTTP endpoint and onto status pills in the operator UI.
const (
	ReasonOK          = "ok"
	ReasonAlreadySent = "already_sent"
	ReasonNoEmail     = "no_email"
)

// ErrNoEmail signals that the client row has no email address — the caller
// should surface a "Add email first" hint to the operator instead of retrying.
var ErrNoEmail = errors.New("email: client has no email address")

// SendDeliverablesParams carries everything SendDeliverables needs.
type SendDeliverablesParams struct {
	JumpID    int64
	TenantID  int64 // tenant scope check; pass session's TenantID
	BaseURL   string
	Force     bool // re-send even when deliverables_email_sent_at IS NOT NULL
}

// SendDeliverablesResult tells the caller whether anything was actually sent.
type SendDeliverablesResult struct {
	Sent      bool
	Reason    string
	Recipient string
	SentAt    time.Time
}

// dataView holds everything the template renders. Built fresh per send.
type dataView struct {
	ClientName  string
	JumpDate    time.Time
	WatchURL    string
	AccessCode  string // dashed for display
	TenantName  string
	OperatorEmail string
	HasMain     bool
	MainLabel   string // "1080p" / "4K"
	HasVertical bool
	HasWOW      bool
	PhotoCount  int
}

// htmlTemplateName must match the filename embedded by web/server/templates.
const htmlTemplateName = "email_deliverables.html"

// SendDeliverables loads jump+client+tenant+artifact set, renders the email,
// hands it to the SMTP sender, and updates jumps.deliverables_email_sent_at
// on success. Idempotent unless params.Force is true.
func SendDeliverables(ctx context.Context, pool *db.Pool, sender *Sender, tpl Renderer, p SendDeliverablesParams) (*SendDeliverablesResult, error) {
	// 1. Load core jump data + tenant guard.
	var (
		clientName   string
		clientEmail  string
		accessCode   string
		jumpDate     time.Time
		tenantName   string
		operatorEmail string
		alreadySent  *time.Time
	)
	err := pool.QueryRow(ctx, `
		SELECT c.name, COALESCE(c.email,''), c.access_code,
		       j.created_at, COALESCE(t.name,''), COALESCE(o.email,''),
		       j.deliverables_email_sent_at
		  FROM jumps j
		  JOIN clients c ON c.id = j.client_id
		  JOIN tenants t ON t.id = j.tenant_id
		  LEFT JOIN operators o ON o.id = j.operator_id
		 WHERE j.id = $1 AND j.tenant_id = $2`,
		p.JumpID, p.TenantID,
	).Scan(&clientName, &clientEmail, &accessCode, &jumpDate, &tenantName, &operatorEmail, &alreadySent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("jump %d not found in tenant %d", p.JumpID, p.TenantID)
	}
	if err != nil {
		return nil, fmt.Errorf("load jump: %w", err)
	}

	// 2. Idempotency guard.
	if alreadySent != nil && !p.Force {
		return &SendDeliverablesResult{
			Sent:      false,
			Reason:    ReasonAlreadySent,
			Recipient: clientEmail,
			SentAt:    *alreadySent,
		}, nil
	}

	// 3. Recipient sanity.
	if strings.TrimSpace(clientEmail) == "" {
		return &SendDeliverablesResult{Sent: false, Reason: ReasonNoEmail}, ErrNoEmail
	}

	// 4. Pull artifact set so the email lists only what's actually delivered.
	view := dataView{
		ClientName:    clientName,
		JumpDate:      jumpDate,
		AccessCode:    dashAccessCode(accessCode),
		TenantName:    tenantName,
		OperatorEmail: operatorEmail,
		WatchURL:      strings.TrimRight(p.BaseURL, "/") + "/watch/" + accessCode,
	}
	rows, qErr := pool.Query(ctx, `
		SELECT kind, height
		  FROM jump_artifacts
		 WHERE jump_id = $1
		   AND kind IN ('horizontal_1080p','horizontal_4k','vertical','wow_highlights','photo')`,
		p.JumpID,
	)
	if qErr == nil {
		defer rows.Close()
		var bestHeight int
		for rows.Next() {
			var kind string
			var height *int
			if scanErr := rows.Scan(&kind, &height); scanErr != nil {
				continue
			}
			switch kind {
			case "horizontal_4k":
				view.HasMain = true
				if height != nil && *height > bestHeight {
					bestHeight = *height
				}
			case "horizontal_1080p":
				view.HasMain = true
				if height != nil && *height > bestHeight {
					bestHeight = *height
				}
			case "vertical":
				view.HasVertical = true
			case "wow_highlights":
				view.HasWOW = true
			case "photo":
				view.PhotoCount++
			}
		}
		view.MainLabel = videoLabelFor(bestHeight)
	}

	// 5. Render HTML body.
	var htmlBuf bytes.Buffer
	if err := tpl.ExecuteTemplate(&htmlBuf, htmlTemplateName, view); err != nil {
		return nil, fmt.Errorf("render html: %w", err)
	}

	// 6. Compose plain-text fallback inline — keeps deliverability up for
	// clients that strip HTML.
	textBody := buildTextFallback(view)

	// 7. Send.
	subject := "Your skydive videos are ready 🪂"
	if err := sender.Send(ctx, clientEmail, subject, htmlBuf.String(), textBody); err != nil {
		return nil, fmt.Errorf("smtp send: %w", err)
	}

	// 8. Persist sent timestamp + short audit message.
	now := time.Now().UTC()
	auditMsg := fmt.Sprintf("Sent to %s at %s", clientEmail, now.Format(time.RFC3339))
	if _, err := pool.Exec(ctx, `
		UPDATE jumps
		   SET deliverables_email_sent_at = $2,
		       deliverables_email_message = $3
		 WHERE id = $1`,
		p.JumpID, now, auditMsg,
	); err != nil {
		// Email already left the building — don't return an error or the
		// caller will retry and double-deliver. Log via the message itself.
		auditMsg = "Sent at " + now.Format(time.RFC3339) + " (DB update failed: " + err.Error() + ")"
	}

	return &SendDeliverablesResult{
		Sent:      true,
		Reason:    ReasonOK,
		Recipient: clientEmail,
		SentAt:    now,
	}, nil
}

func buildTextFallback(v dataView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\n", v.ClientName)
	fmt.Fprintf(&b, "Your skydive on %s is ready to watch and download.\n\n", v.JumpDate.Format("2 Jan 2006"))
	fmt.Fprintf(&b, "Open your private page:\n%s\n\n", v.WatchURL)
	if v.HasMain || v.HasVertical || v.HasWOW || v.PhotoCount > 0 {
		fmt.Fprintln(&b, "What's included:")
		if v.HasMain {
			fmt.Fprintf(&b, "  • Main edit (%s)\n", v.MainLabel)
		}
		if v.HasVertical {
			fmt.Fprintln(&b, "  • Vertical reel for Instagram/TikTok")
		}
		if v.HasWOW {
			fmt.Fprintln(&b, "  • WOW highlights — pure freefall reel")
		}
		if v.PhotoCount > 0 {
			fmt.Fprintf(&b, "  • %d freefall photos\n", v.PhotoCount)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Filmed at %s. Blue skies!\nThe Skydive Memory team\n", v.TenantName)
	return b.String()
}

// dashAccessCode formats "ABCD1234" as "ABCD-1234". Local copy of the
// helper from internal/watch — duplicated here to keep the email package
// dependency-free.
func dashAccessCode(canon string) string {
	if len(canon) == 8 {
		return canon[:4] + "-" + canon[4:]
	}
	return canon
}

// videoLabelFor mirrors the watch.html label logic: render-height → label.
func videoLabelFor(h int) string {
	switch {
	case h >= 2100:
		return "4K"
	case h >= 1400:
		return "2K"
	case h >= 1000:
		return "1080p"
	case h > 0:
		return fmt.Sprintf("%dp", h)
	default:
		return ""
	}
}

// Compile-time assertion: html/template's *Template satisfies Renderer.
var _ Renderer = (*template.Template)(nil)
