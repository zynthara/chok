package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/mail"
	"os"
	"strings"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/internal/conf"
)

// Mailer sends mail: m.Send(ctx, mail.Message{To: …, Template: …, Data: …}).
// Each Send composes the message and hands it to the transport on its
// own connection; there is no pool and no queue — for retries, send from
// a jobs Worker. Register it with app.Use, or not: it only reports a
// name and a health that is always nil.
type Mailer struct {
	cfg       Config
	from      *mail.Address
	replyTo   *mail.Address
	domain    string // From's domain, for Message-IDs
	templates *templates
	logger    *slog.Logger
	deliver   func(ctx context.Context, m *Message, raw []byte) error
	tls       *tls.Config // smtp: the relay's TLS settings
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Option configures New.
type Option func(*options)

type options struct {
	templates    fs.FS
	templatesSet int
	sender       func(ctx context.Context, m *Message, raw []byte) error
	senderSet    bool
	logger       *slog.Logger
}

// Templates gives the mails their templates: in fsys, a template {name}
// is {name}.html, an html/template sent as text/html, {name}.txt, a
// text/template sent as text/plain, or both, sent as
// multipart/alternative. Message names one by {name}, without the
// extension, wherever it sits in fsys (an embed.FS of a templates
// directory works as it is). Files named _*.html, or _*.txt, are partials
// every template of their kind can call; {{define "subject"}}…{{end}} in
// a template is the subject — the .txt one's when both define it —
// unless Message.Subject is set. A template that does not parse fails
// New, and so does giving Templates twice.
func Templates(fsys fs.FS) Option {
	return func(o *options) {
		o.templates = fsys
		o.templatesSet++
	}
}

// Sender replaces the transport, Config.Transport and its settings
// included: fn gets each composed message — m as rendered, its Subject,
// Text and HTML filled from the template — and raw, its RFC 5322 text,
// ready for an HTTP API or a test to take.
func Sender(fn func(ctx context.Context, m *Message, raw []byte) error) Option {
	return func(o *options) { o.sender, o.senderSet = fn, true }
}

// Logger is where the mailer logs; the default is slog.Default(). On a
// request the lines carry its request_id.
func Logger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// New builds a mailer: it checks cfg, what the transport needs and every
// template, and creates transport dir's directory; it does not touch the
// network. A Config from chok.Load carries the defaults; one built in
// code gets them for its zero fields.
func New(cfg Config, opts ...Option) (*Mailer, error) {
	var o options
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("mail: a nil Option")
		}
		opt(&o)
	}
	cfg = cfg.withDefaults()
	if err := conf.Check(&cfg); err != nil {
		return nil, fmt.Errorf("mail: %w", err)
	}
	if o.templatesSet > 1 {
		return nil, errors.New("mail: Templates is given twice: pass one file system with every template")
	}
	if o.templatesSet == 1 && o.templates == nil {
		return nil, errors.New("mail: Templates(nil)")
	}
	if o.senderSet && o.sender == nil {
		return nil, errors.New("mail: Sender(nil)")
	}
	m := &Mailer{cfg: cfg, logger: o.logger}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	m.from, _ = mail.ParseAddress(cfg.From) // Validate parsed it
	m.domain = m.from.Address[strings.LastIndex(m.from.Address, "@")+1:]
	if cfg.ReplyTo != "" {
		m.replyTo, _ = mail.ParseAddress(cfg.ReplyTo)
	}
	if o.templates != nil {
		t, err := parseTemplates(o.templates)
		if err != nil {
			return nil, err
		}
		m.templates = t
	}
	switch {
	case o.sender != nil:
		m.deliver = o.sender
	case cfg.Transport == "dir":
		if cfg.Dir == "" {
			return nil, errors.New("mail: dir is required with transport dir")
		}
		if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("mail: dir: %w", err)
		}
		m.deliver = m.writeFile
	case cfg.Transport == "log":
		m.deliver = m.logOnly
	default:
		if err := m.setupSMTP(); err != nil {
			return nil, err
		}
		m.deliver = m.sendSMTP
	}
	return m, nil
}

// MustNew is New that panics on error.
func MustNew(cfg Config, opts ...Option) *Mailer {
	m, err := New(cfg, opts...)
	if err != nil {
		panic(err)
	}
	return m
}

// Name is "mail", its name in the health report.
func (m *Mailer) Name() string { return "mail" }

// Health is always nil: every Send dials anew, so there is nothing to
// watch between them.
func (m *Mailer) Health(context.Context) error { return nil }

// Send composes msg and delivers it through the transport within
// Config.Timeout. A bad recipient address, a Template that does not
// exist, a Template given with Text or HTML, a header Headers may not
// set: these are plain errors, not chok.Coder ones — a handler wraps them
// in its own. A transport failure wraps the transport's error. Errors
// leave the subject and the recipients out — a subject may carry a
// sign-in code, and errors end up in logs.
func (m *Mailer) Send(ctx context.Context, msg Message) error {
	rendered, err := m.render(msg)
	if err != nil {
		return err
	}
	raw, err := m.compose(rendered)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()
	if err := m.deliver(ctx, rendered, raw); err != nil {
		return fmt.Errorf("mail: send: %w", err)
	}
	return nil
}

// SendTemplate sends one address the template named template, rendered
// with data: Send(ctx, Message{To: []string{to}, Template: template,
// Data: data}). It has the shape of identity's Sender, so a mailer wires
// into identity in one line: identity.Sender(mailer.SendTemplate).
func (m *Mailer) SendTemplate(ctx context.Context, to, template string, data any) error {
	return m.Send(ctx, Message{To: []string{to}, Template: template, Data: data})
}

// log is the mailer's logger, with the request id when ctx has one.
func (m *Mailer) log(ctx context.Context) *slog.Logger {
	if id := chok.RequestID(ctx); id != "" {
		return m.logger.With("request_id", id)
	}
	return m.logger
}
