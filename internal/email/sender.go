// Package email sends transactional mail via SMTP. We use the standard
// library's net/smtp + STARTTLS — no third-party mail dep — and build the
// multipart/alternative payload inline so HTML and plain-text both ship.
//
// Phase 13: deliverables-ready notification with a link to /watch/<code>.
package email

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"mime/quotedprintable"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Config carries everything Sender needs at boot.
type Config struct {
	Host     string // smtp.resend.com or localhost
	Port     int    // 587 (Resend STARTTLS) or 51025 (MailHog plain)
	Username string // "resend" for Resend; empty for MailHog
	Password string // API key; empty for MailHog
	From     string // "Display Name <addr@domain>"
}

// Sender is the package's only public type. Constructed once at boot.
type Sender struct{ cfg Config }

// New returns a Sender. A zero-Host config is valid — Send becomes a no-op
// and logs a warning, useful for local runs without an SMTP relay.
func New(cfg Config) *Sender { return &Sender{cfg: cfg} }

// Configured reports whether the sender will actually attempt a delivery.
func (s *Sender) Configured() bool { return s.cfg.Host != "" }

// ErrNotConfigured indicates the sender was used without an SMTP host.
var ErrNotConfigured = errors.New("email: SMTP host not configured")

// Send delivers a multipart/alternative message via SMTP. Uses STARTTLS
// when the relay supports it (port 587 / Resend); falls back to plain SMTP
// for MailHog (port 51025). Auth is PLAIN with the configured creds.
//
// `to` may be a bare address ("user@host") or formatted ("Name <user@host>").
func (s *Sender) Send(ctx context.Context, to, subject, htmlBody, textBody string) error {
	if !s.Configured() {
		log.Printf("email.Send: SMTP_HOST blank — would send to=%q subject=%q (skipped)", to, subject)
		return ErrNotConfigured
	}
	if to == "" {
		return errors.New("email: empty recipient")
	}

	msg, err := buildMessage(s.cfg.From, to, subject, htmlBody, textBody)
	if err != nil {
		return fmt.Errorf("build message: %w", err)
	}

	addr := s.cfg.Host + ":" + strconv.Itoa(s.cfg.Port)

	// Honour the caller's deadline by closing the dialer in a goroutine
	// when ctx fires. net/smtp doesn't take a context directly.
	type sendResult struct{ err error }
	done := make(chan sendResult, 1)
	go func() {
		done <- sendResult{err: deliver(addr, s.cfg.Host, s.cfg.Username, s.cfg.Password,
			fromAddr(s.cfg.From), []string{toAddr(to)}, msg)}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-done:
		return r.err
	}
}

// deliver opens an SMTP connection, optionally upgrades to TLS, authenticates
// (when credentials are present), and submits the message.
func deliver(addr, host, username, password, from string, to []string, msg []byte) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}

	if username != "" || password != "" {
		auth := smtp.PlainAuth("", username, password, host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}

	if err := c.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, addr := range to {
		if err := c.Rcpt(addr); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", addr, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		_ = wc.Close()
		return fmt.Errorf("write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("close DATA: %w", err)
	}
	return c.Quit()
}

// buildMessage assembles RFC 5322 headers + multipart/alternative body
// (text first, HTML second — last part wins per RFC, modern clients render
// HTML and fall back to text when HTML is blocked).
func buildMessage(from, to, subject, htmlBody, textBody string) ([]byte, error) {
	boundary, err := randomBoundary()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", from)
	fmt.Fprintf(&buf, "To: %s\r\n", to)
	fmt.Fprintf(&buf, "Subject: %s\r\n", encodeHeader(subject))
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%q\r\n", boundary)
	fmt.Fprintf(&buf, "\r\n")

	if textBody == "" {
		textBody = "Your videos are ready."
	}
	if err := writePart(&buf, boundary, "text/plain; charset=UTF-8", textBody); err != nil {
		return nil, err
	}
	if err := writePart(&buf, boundary, "text/html; charset=UTF-8", htmlBody); err != nil {
		return nil, err
	}

	fmt.Fprintf(&buf, "--%s--\r\n", boundary)
	return buf.Bytes(), nil
}

func writePart(buf *bytes.Buffer, boundary, contentType, body string) error {
	fmt.Fprintf(buf, "--%s\r\n", boundary)
	fmt.Fprintf(buf, "Content-Type: %s\r\n", contentType)
	fmt.Fprintf(buf, "Content-Transfer-Encoding: quoted-printable\r\n")
	fmt.Fprintf(buf, "\r\n")
	w := quotedprintable.NewWriter(buf)
	if _, err := w.Write([]byte(body)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	fmt.Fprintf(buf, "\r\n")
	return nil
}

// encodeHeader RFC 2047-encodes a header value when it contains non-ASCII.
// Skydive Memory subjects are typically pure ASCII; this guards against
// emoji or accented client names slipping through.
func encodeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}

// randomBoundary returns a 24-char URL-safe boundary token.
func randomBoundary() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sdm-" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// toAddr extracts "addr@host" from "Name <addr@host>" or returns the input
// when no angle brackets are present.
func toAddr(formatted string) string {
	if i := strings.LastIndex(formatted, "<"); i >= 0 {
		if j := strings.Index(formatted[i:], ">"); j > 0 {
			return formatted[i+1 : i+j]
		}
	}
	return strings.TrimSpace(formatted)
}

// fromAddr is identical to toAddr today; kept as a separate symbol so a
// future change (e.g. envelope-from differing from header-from for
// bounce handling) doesn't have to chase usages.
func fromAddr(formatted string) string { return toAddr(formatted) }
