package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is the smallest SMTP server the transport needs: greeting,
// EHLO, STARTTLS, AUTH PLAIN, MAIL, RCPT, DATA and QUIT, on a random
// loopback port, recording each session.
type fakeSMTP struct {
	t        *testing.T
	ln       net.Listener
	port     int
	cert     tls.Certificate
	pool     *x509.CertPool
	startTLS bool   // offer STARTTLS
	silent   bool   // accept and never answer
	reject   string // a RCPT address answered 550

	mu       sync.Mutex
	sessions []*smtpSession
	done     sync.WaitGroup
}

type smtpSession struct {
	commands []string // the verbs, in order
	tls      bool     // DATA arrived over TLS
	auth     string   // the decoded AUTH PLAIN response
	from     string
	rcpt     []string
	data     string
}

// newFakeSMTP listens on 127.0.0.1:0, speaking TLS from the first byte
// when implicit.
func newFakeSMTP(t *testing.T, implicit bool, configure func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	s := &fakeSMTP{t: t}
	s.cert, s.pool = selfSigned(t)
	if configure != nil {
		configure(s)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if implicit {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{s.cert}})
	}
	s.ln = ln
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.done.Add(1)
	go s.serve(implicit)
	t.Cleanup(func() {
		_ = ln.Close()
		s.done.Wait()
	})
	return s
}

func (s *fakeSMTP) serve(implicit bool) {
	defer s.done.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		sess := &smtpSession{tls: implicit}
		s.mu.Lock()
		s.sessions = append(s.sessions, sess)
		s.mu.Unlock()
		s.done.Go(func() {
			defer func() { _ = conn.Close() }()
			s.session(conn, sess)
		})
	}
}

func (s *fakeSMTP) session(conn net.Conn, sess *smtpSession) {
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if s.silent {
		_, _ = bufio.NewReader(conn).ReadString(0) // until the client gives up
		return
	}
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	reply := func(lines ...string) {
		for _, l := range lines {
			_, _ = w.WriteString(l + "\r\n")
		}
		_ = w.Flush()
	}
	record := func(f func()) {
		s.mu.Lock()
		defer s.mu.Unlock()
		f()
	}
	reply("220 fake.test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		record(func() { sess.commands = append(sess.commands, verb) })
		switch verb {
		case "EHLO", "HELO":
			ext := []string{"250-fake.test greets " + arg}
			if s.startTLS && !sess.tls {
				ext = append(ext, "250-STARTTLS")
			}
			reply(append(ext, "250-AUTH PLAIN", "250 8BITMIME")...)
		case "STARTTLS":
			reply("220 go ahead")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, r, w = tc, bufio.NewReader(tc), bufio.NewWriter(tc)
			record(func() { sess.tls = true })
		case "AUTH":
			mech, resp, _ := strings.Cut(arg, " ")
			b, err := base64.StdEncoding.DecodeString(resp)
			if mech != "PLAIN" || err != nil {
				reply("535 no")
				continue
			}
			record(func() { sess.auth = string(b) })
			reply("235 accepted")
		case "MAIL":
			record(func() { sess.from = arg })
			reply("250 ok")
		case "RCPT":
			if s.reject != "" && strings.Contains(arg, s.reject) {
				reply("550 no such user")
				continue
			}
			record(func() { sess.rcpt = append(sess.rcpt, arg) })
			reply("250 ok")
		case "DATA":
			reply("354 end with <CRLF>.<CRLF>")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, "."))
			}
			record(func() { sess.data = data.String() })
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 unknown command")
		}
	}
}

// only is a copy of the one session the server had.
func (s *fakeSMTP) only(t *testing.T) smtpSession {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) != 1 {
		t.Fatalf("%d sessions", len(s.sessions))
	}
	sess := *s.sessions[0]
	sess.commands, sess.rcpt = slices.Clone(sess.commands), slices.Clone(sess.rcpt)
	return sess
}

// selfSigned is a certificate for 127.0.0.1 and localhost, and the pool
// that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// smtpMailer is a mailer that sends through s, trusting its certificate.
func smtpMailer(t *testing.T, s *fakeSMTP, c SMTPConfig, timeout time.Duration) *Mailer {
	t.Helper()
	c.Host, c.Port = "127.0.0.1", s.port
	m, err := New(Config{From: testFrom, Timeout: timeout, SMTP: c})
	if err != nil {
		t.Fatal(err)
	}
	m.tls.RootCAs = s.pool
	return m
}

var bcc = Message{
	To: []string{"Ada <ada@example.com>"}, Cc: []string{"carol@example.com"}, Bcc: []string{"audit@shop.example"},
	Subject: "Order", Text: "shipped\n.dot line\n",
}

// TestSMTP_StartTLS: EHLO, STARTTLS, EHLO again, AUTH PLAIN over TLS,
// MAIL FROM, a RCPT per recipient — Bcc included — DATA without Bcc, and
// QUIT.
func TestSMTP_StartTLS(t *testing.T) {
	s := newFakeSMTP(t, false, func(s *fakeSMTP) { s.startTLS = true })
	m := smtpMailer(t, s, SMTPConfig{Username: "apikey", Password: "relay-secret", TLS: "starttls"}, 10*time.Second)
	if err := m.Send(t.Context(), bcc); err != nil {
		t.Fatal(err)
	}
	sess := s.only(t)
	if want := []string{"EHLO", "STARTTLS", "EHLO", "AUTH", "MAIL", "RCPT", "RCPT", "RCPT", "DATA", "QUIT"}; !slices.Equal(sess.commands, want) {
		t.Fatalf("commands %v, want %v", sess.commands, want)
	}
	if !sess.tls || sess.auth != "\x00apikey\x00relay-secret" || !strings.HasPrefix(sess.from, "FROM:<no-reply@shop.example>") {
		t.Fatalf("session %+v", sess)
	}
	if want := []string{"TO:<ada@example.com>", "TO:<carol@example.com>", "TO:<audit@shop.example>"}; !slices.Equal(sess.rcpt, want) {
		t.Fatalf("RCPT %v", sess.rcpt)
	}
	if strings.Contains(sess.data, "audit@shop.example") || !strings.Contains(sess.data, "Subject: Order\r\n") || !strings.Contains(sess.data, "\r\n.dot line\r\n") {
		t.Fatalf("DATA:\n%s", sess.data)
	}
}

// TestSMTP_ImplicitTLS: port 465 style, TLS from the first byte.
func TestSMTP_ImplicitTLS(t *testing.T) {
	s := newFakeSMTP(t, true, nil)
	m := smtpMailer(t, s, SMTPConfig{Username: "u", Password: "p", TLS: "implicit"}, 10*time.Second)
	if err := m.Send(t.Context(), bcc); err != nil {
		t.Fatal(err)
	}
	sess := s.only(t)
	if !sess.tls || sess.auth != "\x00u\x00p" || slices.Contains(sess.commands, "STARTTLS") || len(sess.rcpt) != 3 {
		t.Fatalf("session %+v", sess)
	}
}

// TestSMTP_NoneOnLoopback: cleartext to a local relay, with or without
// credentials.
func TestSMTP_NoneOnLoopback(t *testing.T) {
	s := newFakeSMTP(t, false, nil)
	m := smtpMailer(t, s, SMTPConfig{TLS: "none"}, 10*time.Second)
	if err := m.Send(t.Context(), bcc); err != nil {
		t.Fatal(err)
	}
	if sess := s.only(t); sess.tls || sess.auth != "" || slices.Contains(sess.commands, "AUTH") || sess.data == "" {
		t.Fatalf("session %+v", sess)
	}
}

// TestSMTP_StartTLSRequired: a relay that does not offer STARTTLS gets
// neither the credentials nor the mail.
func TestSMTP_StartTLSRequired(t *testing.T) {
	s := newFakeSMTP(t, false, nil)
	m := smtpMailer(t, s, SMTPConfig{Username: "u", Password: "p", TLS: "starttls"}, 10*time.Second)
	err := m.Send(t.Context(), bcc)
	if err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatalf("Send: %v", err)
	}
	if sess := s.only(t); sess.auth != "" || sess.data != "" {
		t.Fatalf("session %+v", sess)
	}
}

// TestSMTP_RejectedRecipient: a refused RCPT fails the Send and names
// the address.
func TestSMTP_RejectedRecipient(t *testing.T) {
	s := newFakeSMTP(t, false, func(s *fakeSMTP) { s.reject = "carol@" })
	m := smtpMailer(t, s, SMTPConfig{TLS: "none"}, 10*time.Second)
	err := m.Send(t.Context(), bcc)
	if err == nil || !strings.Contains(err.Error(), "RCPT TO carol@example.com") || !strings.Contains(err.Error(), "550") {
		t.Fatalf("Send: %v", err)
	}
}

// TestSMTP_Timeout: a relay that never answers holds a Send no longer
// than Config.Timeout, and the error says so; a canceled context ends it
// too.
func TestSMTP_Timeout(t *testing.T) {
	s := newFakeSMTP(t, false, func(s *fakeSMTP) { s.silent = true })
	m := smtpMailer(t, s, SMTPConfig{TLS: "none"}, 300*time.Millisecond)
	start := time.Now()
	err := m.Send(t.Context(), bcc)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("Send after %s: %v", time.Since(start), err)
	}
	m.cfg.Timeout = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start = time.Now()
	if err := m.Send(ctx, bcc); !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("canceled Send after %s: %v", time.Since(start), err)
	}
	// A relay that is not there fails at once.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	m.cfg.SMTP.Port = port
	if err := m.Send(t.Context(), bcc); err == nil || !strings.Contains(err.Error(), "127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("no relay: %v", err)
	}
}

// lateContext has a deadline that passed, which it has not noticed yet:
// a context's timer can run after the connection's deadline, set to the
// same instant, has failed a read.
type lateContext struct{ context.Context }

func (lateContext) Deadline() (time.Time, bool) { return time.Now().Add(-time.Millisecond), true }

// TestWithContext_NamesWhyTheSendEnded: a failure after ctx's deadline is
// context.DeadlineExceeded even before ctx says so, a canceled ctx's is
// context.Canceled, and a failure of a live ctx stays as it is.
func TestWithContext_NamesWhyTheSendEnded(t *testing.T) {
	timeout := errors.New("read tcp 127.0.0.1:1: i/o timeout")
	if err := withContext(lateContext{context.Background()}, timeout); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, timeout) {
		t.Fatalf("past the deadline: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := withContext(canceled, timeout); !errors.Is(err, context.Canceled) || !errors.Is(err, timeout) {
		t.Fatalf("canceled: %v", err)
	}
	if err := withContext(t.Context(), timeout); err != timeout {
		t.Fatalf("a live context: %v", err)
	}
	if err := withContext(canceled, nil); err != nil {
		t.Fatalf("no error: %v", err)
	}
	// Already named: not twice.
	if err := withContext(canceled, fmt.Errorf("dial: %w", context.Canceled)); strings.Count(err.Error(), "canceled") != 1 {
		t.Fatalf("named twice: %v", err)
	}
}
