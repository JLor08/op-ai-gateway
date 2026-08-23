// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package mail sends transactional email — currently the user
// activation/invite link and an SMTP self-test — over SMTP using only the Go
// standard library (net/smtp + crypto/tls + net). No third-party dependency is
// introduced (repository license policy: permissive stdlib only). A Mailer is
// built from a Config and speaks one of three TLS modes; templates.go supplies
// the localized message content.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// TLS modes accepted in Config.TLSMode.
const (
	TLSModeStartTLS = "starttls" // plain dial, upgrade with STARTTLS, then AUTH
	TLSModeSSL      = "ssl"      // implicit TLS from the first byte (SMTPS)
	TLSModeNone     = "none"     // plaintext; AUTH only via the unencrypted wrapper
)

// dialTimeout bounds the whole dial + handshake + send exchange when the caller
// supplies no (or a later) context deadline.
const dialTimeout = 10 * time.Second

// ErrHostRequired is returned by Send when Config.Host is empty.
var ErrHostRequired = errors.New("mail: smtp host required")

// ErrInvalidTLSMode is returned by Send when Config.TLSMode is not one of the
// three known modes.
var ErrInvalidTLSMode = errors.New("mail: invalid tls mode")

// Config describes an SMTP relay. Username/Password are optional: an empty
// Username means no AUTH is attempted (open internal relays). From is the
// envelope + header From address; FromName is an optional display name.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string
	TLSMode  string // "starttls" | "ssl" | "none"
}

// Mailer sends messages through a fixed Config.
type Mailer struct {
	cfg Config

	// tlsConfig, when non-nil, is cloned as the base client TLS config (its
	// ServerName defaults to the host). Only tests set it, to trust a
	// self-signed in-process server; production always uses ServerName-only
	// verification against the system roots.
	tlsConfig *tls.Config
}

// New returns a Mailer bound to cfg.
func New(cfg Config) *Mailer { return &Mailer{cfg: cfg} }

// Send delivers one UTF-8 text/plain message to a single recipient. The whole
// exchange is bounded by ~10s or the context deadline, whichever is sooner. It
// returns the first protocol error and never blocks indefinitely.
func (m *Mailer) Send(ctx context.Context, to, subject, body string) error {
	if strings.TrimSpace(m.cfg.Host) == "" {
		return ErrHostRequired
	}
	switch m.cfg.TLSMode {
	case TLSModeStartTLS, TLSModeSSL, TLSModeNone:
	default:
		return ErrInvalidTLSMode
	}

	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	c, err := m.dial(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := m.auth(c); err != nil {
		return err
	}
	if err := c.Mail(m.cfg.From); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(m.buildMessage(to, subject, body)); err != nil {
		return fmt.Errorf("mail: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: close body: %w", err)
	}
	return c.Quit()
}

// dial opens an smtp.Client for the configured TLS mode, applying the effective
// deadline to both the dial and the live connection.
func (m *Mailer) dial(ctx context.Context, addr string) (*smtp.Client, error) {
	deadline := time.Now().Add(dialTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Deadline: deadline}

	switch m.cfg.TLSMode {
	case TLSModeSSL:
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, m.clientTLSConfig())
		if err != nil {
			return nil, fmt.Errorf("mail: tls dial: %w", err)
		}
		_ = conn.SetDeadline(deadline)
		c, err := smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("mail: smtp client: %w", err)
		}
		return c, nil
	case TLSModeStartTLS:
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("mail: dial: %w", err)
		}
		_ = conn.SetDeadline(deadline)
		c, err := smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("mail: smtp client: %w", err)
		}
		if err := c.StartTLS(m.clientTLSConfig()); err != nil {
			c.Close()
			return nil, fmt.Errorf("mail: starttls: %w", err)
		}
		return c, nil
	default: // TLSModeNone
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("mail: dial: %w", err)
		}
		_ = conn.SetDeadline(deadline)
		c, err := smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("mail: smtp client: %w", err)
		}
		return c, nil
	}
}

// clientTLSConfig builds the TLS config used for SSL dial and STARTTLS upgrade.
func (m *Mailer) clientTLSConfig() *tls.Config {
	if m.tlsConfig != nil {
		c := m.tlsConfig.Clone()
		if c.ServerName == "" {
			c.ServerName = m.cfg.Host
		}
		return c
	}
	return &tls.Config{ServerName: m.cfg.Host}
}

// auth issues AUTH when a username is set and the server advertises it. In the
// "none" (plaintext) mode net/smtp's PlainAuth refuses to transmit credentials;
// the unencryptedAuth wrapper opts in for trusted internal relays. TLS/SSL and
// STARTTLS already satisfy PlainAuth's transport check.
func (m *Mailer) auth(c *smtp.Client) error {
	if m.cfg.Username == "" {
		return nil
	}
	if ok, _ := c.Extension("AUTH"); !ok {
		return nil
	}
	a := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
	if m.cfg.TLSMode == TLSModeNone {
		a = unencryptedAuth{a}
	}
	if err := c.Auth(a); err != nil {
		return fmt.Errorf("mail: auth: %w", err)
	}
	return nil
}

// unencryptedAuth wraps an smtp.Auth so PlainAuth proceeds on a non-TLS
// connection (net/smtp otherwise aborts to avoid leaking credentials). Used
// only for TLSModeNone against trusted internal relays.
type unencryptedAuth struct {
	smtp.Auth
}

func (a unencryptedAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	s := *server
	s.TLS = true
	return a.Auth.Start(&s)
}

// buildMessage renders an RFC 5322 message: From (display name quoted +
// RFC2047-encoded via mail.Address when a name is set), To, RFC2047-encoded
// Subject, Date, MIME-Version, a UTF-8 text/plain Content-Type, then the
// quoted-printable-encoded, CRLF-normalized body after a blank line. The body
// is quoted-printable (not raw 8bit) so non-ASCII text — e.g. the default
// German templates' ü/ö/ä/ß — survives a strict 7-bit-only SMTP relay
// regardless of whether it advertises 8BITMIME.
func (m *Mailer) buildMessage(to, subject, body string) []byte {
	from := m.cfg.From
	if m.cfg.FromName != "" {
		from = (&mail.Address{Name: m.cfg.FromName, Address: m.cfg.From}).String()
	}
	var b strings.Builder
	writeHeader(&b, "From", from)
	writeHeader(&b, "To", to)
	writeHeader(&b, "Subject", mime.QEncoding.Encode("utf-8", subject))
	writeHeader(&b, "Date", time.Now().Format(time.RFC1123Z))
	writeHeader(&b, "MIME-Version", "1.0")
	writeHeader(&b, "Content-Type", "text/plain; charset=utf-8")
	writeHeader(&b, "Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(normalizeCRLF(body)))
	_ = qp.Close()
	return []byte(b.String())
}

func writeHeader(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\r\n")
}

// normalizeCRLF converts any bare LF (and existing CRLF) to a single canonical
// CRLF so the SMTP DATA payload never carries lone LFs.
func normalizeCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
