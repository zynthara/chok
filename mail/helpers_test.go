package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"sync"
	"testing"
)

const testFrom = "Shop <no-reply@shop.example>"

// captured is what a Sender option saw.
type captured struct {
	mu   sync.Mutex
	msgs []*Message
	raws [][]byte
	ctxs []context.Context
}

func (c *captured) send(ctx context.Context, m *Message, raw []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	c.raws = append(c.raws, raw)
	c.ctxs = append(c.ctxs, ctx)
	return nil
}

func (c *captured) last(t *testing.T) (*Message, []byte) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.msgs) == 0 {
		t.Fatal("nothing was sent")
	}
	return c.msgs[len(c.msgs)-1], c.raws[len(c.raws)-1]
}

// capture builds a mailer whose transport is c.
func capture(t *testing.T, cfg Config, opts ...Option) (*Mailer, *captured) {
	t.Helper()
	if cfg.From == "" {
		cfg.From = testFrom
	}
	c := &captured{}
	m, err := New(cfg, append(opts, Sender(c.send))...)
	if err != nil {
		t.Fatal(err)
	}
	return m, c
}

// entity is a parsed MIME entity: its header, its decoded body, and its
// parts when it is a multipart.
type entity struct {
	header    map[string][]string
	mediaType string
	params    map[string]string
	body      string
	parts     []*entity
}

func (e *entity) get(k string) string {
	if v := e.header[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// parseMail reads raw as RFC 5322 and decodes its MIME tree.
func parseMail(t *testing.T, raw []byte) *entity {
	t.Helper()
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatalf("a bare line feed in the message:\n%s", raw)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("read the message: %v\n%s", err, raw)
	}
	e := &entity{header: msg.Header}
	decodeEntity(t, e, msg.Body)
	return e
}

func decodeEntity(t *testing.T, e *entity, body io.Reader) {
	t.Helper()
	ct := e.get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("content type %q: %v", ct, err)
	}
	e.mediaType, e.params = mt, params
	if strings.HasPrefix(mt, "multipart/") {
		r := multipart.NewReader(body, params["boundary"])
		for {
			p, err := r.NextRawPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatalf("the parts of %s: %v", mt, err)
			}
			child := &entity{header: p.Header}
			decodeEntity(t, child, p)
			e.parts = append(e.parts, child)
		}
	}
	r := body
	switch e.get("Content-Transfer-Encoding") {
	case "quoted-printable":
		r = quotedprintable.NewReader(body)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, body)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decode %s: %v", mt, err)
	}
	e.body = string(b)
}

// syncBuffer is a log destination tests read while the mailer writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func debugLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
