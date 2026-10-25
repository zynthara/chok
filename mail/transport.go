package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// setupSMTP checks the relay settings and prepares the TLS settings and
// the dialer every Send uses.
func (m *Mailer) setupSMTP() error {
	c := m.cfg.SMTP
	if c.Host == "" {
		return errors.New("mail: smtp.host is required with transport smtp")
	}
	if c.TLS == "none" && !isLoopback(c.Host) {
		return fmt.Errorf("mail: smtp.tls none sends in the clear: only a loopback host may use it, not %s", c.Host)
	}
	m.tls = &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	d := &net.Dialer{}
	m.dial = d.DialContext
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sendSMTP delivers raw through the relay on a connection of its own:
// dial, EHLO, STARTTLS (or TLS from the start), AUTH PLAIN, MAIL, a RCPT
// per recipient (Bcc included), DATA, QUIT — all within ctx. A send that
// ctx ended fails with ctx's error as well as the connection's.
func (m *Mailer) sendSMTP(ctx context.Context, msg *Message, raw []byte) error {
	return withContext(ctx, m.smtpSession(ctx, msg, raw))
}

// withContext adds to err the reason ctx ended, when it did: its deadline
// passed — which the connection's deadline, set to the same instant, may
// report before ctx's own timer has marked it done — or it was canceled.
func withContext(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	cause := ctx.Err()
	if cause == nil {
		if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
			cause = context.DeadlineExceeded
		}
	}
	if cause == nil || errors.Is(err, cause) {
		return err
	}
	return fmt.Errorf("%w (%w)", cause, err)
}

func (m *Mailer) smtpSession(ctx context.Context, msg *Message, raw []byte) error {
	rcpt, err := recipients(msg)
	if err != nil {
		return err
	}
	c := m.cfg.SMTP
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	conn, err := m.dial(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp %s: %w", addr, err)
	}
	// The deadline bounds every read and write; a canceled ctx cuts the
	// connection short.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if c.TLS == "implicit" {
		tc := tls.Client(conn, m.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("smtp %s: TLS: %w", addr, err)
		}
		conn = tc
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp %s: %w", addr, err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Hello("localhost"); err != nil {
		return fmt.Errorf("smtp %s: EHLO: %w", addr, err)
	}
	if c.TLS == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("smtp %s does not offer STARTTLS: set mail.smtp.tls to implicit for a TLS port, or none for a local one", addr)
		}
		if err := client.StartTLS(m.tls); err != nil {
			return fmt.Errorf("smtp %s: STARTTLS: %w", addr, err)
		}
	}
	if c.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return fmt.Errorf("smtp %s: AUTH: %w", addr, err)
		}
	}
	if err := client.Mail(m.from.Address); err != nil {
		return fmt.Errorf("smtp %s: MAIL FROM: %w", addr, err)
	}
	for _, to := range rcpt {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("smtp %s: RCPT TO %s: %w", addr, to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp %s: DATA: %w", addr, err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("smtp %s: DATA: %w", addr, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp %s: DATA: %w", addr, err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("smtp %s: QUIT: %w", addr, err)
	}
	m.log(ctx).DebugContext(ctx, "mail: sent", "to", strings.Join(msg.To, ", "), "subject", msg.Subject, "relay", addr)
	return nil
}

// writeFile is transport dir: raw becomes {Dir}/{unix ns}-{random}.eml,
// written under a temporary name and renamed, so a reader never sees half
// a mail.
func (m *Mailer) writeFile(ctx context.Context, msg *Message, raw []byte) error {
	name := strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + randomHex(4) + ".eml"
	tmp, err := os.CreateTemp(m.cfg.Dir, ".mail-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	p := filepath.Join(m.cfg.Dir, name)
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	m.log(ctx).InfoContext(ctx, "mail: written", "path", p, "to", strings.Join(msg.To, ", "), "subject", msg.Subject)
	return nil
}

// logOnly is transport log: the mail goes to the log, its body at Debug.
func (m *Mailer) logOnly(ctx context.Context, msg *Message, raw []byte) error {
	l := m.log(ctx)
	l.InfoContext(ctx, "mail: logged, not sent", "to", strings.Join(msg.To, ", "), "subject", msg.Subject, "bytes", len(raw))
	l.DebugContext(ctx, "mail: body", "text", msg.Text, "html", msg.HTML)
	return nil
}
