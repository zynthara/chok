package mail

import (
	"mime"
	"net/mail"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCompose_TextOnly: one text part, quoted-printable, under the
// header fields every mail carries; Bcc is delivered but never shown.
func TestCompose_TextOnly(t *testing.T) {
	m, c := capture(t, Config{ReplyTo: "Support <help@shop.example>"})
	err := m.Send(t.Context(), Message{
		To: []string{"Ada <ada@example.com>", "bob@example.com"}, Cc: []string{"carol@example.com"}, Bcc: []string{"audit@shop.example"},
		Subject: "Your order", Text: "Hello Ada,\nyour order shipped.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, raw := c.last(t)
	e := parseMail(t, raw)
	for k, want := range map[string]string{
		"From":                      `"Shop" <no-reply@shop.example>`,
		"Reply-To":                  `"Support" <help@shop.example>`,
		"To":                        `"Ada" <ada@example.com>, <bob@example.com>`,
		"Cc":                        `<carol@example.com>`,
		"Subject":                   "Your order",
		"Mime-Version":              "1.0",
		"Content-Type":              "text/plain; charset=utf-8",
		"Content-Transfer-Encoding": "quoted-printable",
	} {
		if got := e.get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
	if e.get("Bcc") != "" || strings.Contains(string(raw), "audit@shop.example") {
		t.Fatalf("Bcc shows in the message:\n%s", raw)
	}
	if at, err := mail.ParseDate(e.get("Date")); err != nil || time.Since(at) > time.Minute {
		t.Fatalf("Date %q: %v", e.get("Date"), err)
	}
	if e.body != "Hello Ada,\r\nyour order shipped.\r\n" {
		t.Fatalf("body %q", e.body)
	}
}

// TestCompose_MessageIDAtFromsDomain: a fresh Message-ID per mail, at the
// sender's domain.
func TestCompose_MessageIDAtFromsDomain(t *testing.T) {
	m, c := capture(t, Config{})
	for range 2 {
		if err := m.Send(t.Context(), Message{To: []string{"ada@example.com"}, Text: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]bool{}
	for _, raw := range c.raws {
		id := parseMail(t, raw).get("Message-Id")
		if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@shop.example>") || len(id) < 40 {
			t.Fatalf("Message-ID %q", id)
		}
		ids[id] = true
	}
	if len(ids) != 2 {
		t.Fatal("two mails share a Message-ID")
	}
}

// TestCompose_Alternative: text and HTML go out as multipart/alternative,
// the plain text first.
func TestCompose_Alternative(t *testing.T) {
	m, c := capture(t, Config{})
	if err := m.Send(t.Context(), Message{To: []string{"ada@example.com"}, Subject: "s", Text: "plain", HTML: "<p>rich</p>"}); err != nil {
		t.Fatal(err)
	}
	_, raw := c.last(t)
	e := parseMail(t, raw)
	if e.mediaType != "multipart/alternative" || len(e.parts) != 2 {
		t.Fatalf("%s with %d parts", e.mediaType, len(e.parts))
	}
	if p := e.parts[0]; p.mediaType != "text/plain" || p.params["charset"] != "utf-8" || p.body != "plain" {
		t.Fatalf("first part %s %q", p.mediaType, p.body)
	}
	if p := e.parts[1]; p.mediaType != "text/html" || p.body != "<p>rich</p>" || p.get("Content-Transfer-Encoding") != "quoted-printable" {
		t.Fatalf("second part %s %q", p.mediaType, p.body)
	}
}

// TestCompose_MixedRelated: inline images join the HTML in
// multipart/related, attachments wrap everything in multipart/mixed.
func TestCompose_MixedRelated(t *testing.T) {
	m, c := capture(t, Config{})
	logo := []byte("\x89PNG fake image bytes")
	pdf := []byte(strings.Repeat("%PDF-1.7 invoice ", 20)) // longer than one base64 line
	err := m.Send(t.Context(), Message{
		To: []string{"ada@example.com"}, Subject: "Invoice",
		Text: "See the invoice.", HTML: `<img src="cid:logo.png"> See the invoice.`,
		Attachments: []Attachment{
			{Name: "logo.png", Content: logo, Inline: true},
			{Name: "invoice.pdf", Content: pdf},
			{Name: "notes.unknownext", Content: []byte("x")},
			{Name: "data.csv", ContentType: "text/csv", Content: []byte("a,b\n")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, raw := c.last(t)
	e := parseMail(t, raw)
	if e.mediaType != "multipart/mixed" || len(e.parts) != 4 {
		t.Fatalf("top: %s with %d parts", e.mediaType, len(e.parts))
	}
	related := e.parts[0]
	if related.mediaType != "multipart/related" || len(related.parts) != 2 {
		t.Fatalf("first: %s with %d parts", related.mediaType, len(related.parts))
	}
	if alt := related.parts[0]; alt.mediaType != "multipart/alternative" || len(alt.parts) != 2 {
		t.Fatalf("related's first: %s", alt.mediaType)
	}
	img := related.parts[1]
	if img.mediaType != "image/png" || img.get("Content-Id") != "<logo.png>" || !strings.HasPrefix(img.get("Content-Disposition"), "inline") || img.body != string(logo) {
		t.Fatalf("inline image: %v %q", img.header, img.body)
	}
	for i, want := range []struct{ mediaType, name, body string }{
		{"application/pdf", "invoice.pdf", string(pdf)},
		{"application/octet-stream", "notes.unknownext", "x"},
		{"text/csv", "data.csv", "a,b\n"},
	} {
		p := e.parts[i+1]
		_, disp, _ := mime.ParseMediaType(p.get("Content-Disposition"))
		if p.mediaType != want.mediaType || p.params["name"] != want.name || disp["filename"] != want.name || p.body != want.body ||
			p.get("Content-Transfer-Encoding") != "base64" || !strings.HasPrefix(p.get("Content-Disposition"), "attachment") {
			t.Errorf("attachment %d: %v %q", i, p.header, p.body)
		}
	}
	for line := range strings.Lines(string(raw)) {
		if len(strings.TrimRight(line, "\r\n")) > 998 {
			t.Fatalf("a line of %d characters", len(line))
		}
	}
	// Inline images alone make multipart/related, attachments alone
	// multipart/mixed.
	if err := m.Send(t.Context(), Message{To: []string{"a@example.com"}, HTML: "x", Attachments: []Attachment{{Name: "a.png", Content: logo, Inline: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, raw := c.last(t); parseMail(t, raw).mediaType != "multipart/related" {
		t.Fatal("HTML with an inline image")
	}
	if err := m.Send(t.Context(), Message{To: []string{"a@example.com"}, Text: "x", Attachments: []Attachment{{Name: "a.txt", Content: []byte("y")}}}); err != nil {
		t.Fatal(err)
	}
	if _, raw := c.last(t); parseMail(t, raw).mediaType != "multipart/mixed" {
		t.Fatal("text with an attachment")
	}
}

// TestCompose_QuotedPrintable: long lines wrap softly at 76, non-ASCII
// and = are escaped, and the text decodes to itself.
func TestCompose_QuotedPrintable(t *testing.T) {
	m, c := capture(t, Config{})
	text := strings.Repeat("Grüße aus Zürich, 1 + 1 = 2. ", 8) + "\n中文的一行\nend  "
	if err := m.Send(t.Context(), Message{To: []string{"a@example.com"}, Text: text}); err != nil {
		t.Fatal(err)
	}
	_, raw := c.last(t)
	head, body, _ := strings.Cut(string(raw), "\r\n\r\n")
	if !strings.Contains(head, "Content-Transfer-Encoding: quoted-printable") {
		t.Fatal(head)
	}
	for line := range strings.Lines(body) {
		if l := strings.TrimRight(line, "\r\n"); len(l) > 76 {
			t.Fatalf("a quoted-printable line of %d: %q", len(l), l)
		}
	}
	if !strings.Contains(body, "=C3=BC") || !strings.Contains(body, "=3D") || !strings.Contains(body, "=\r\n") {
		t.Fatalf("the encoding:\n%s", body)
	}
	if got := parseMail(t, raw).body; got != strings.ReplaceAll(text, "\n", "\r\n") {
		t.Fatalf("decoded %q", got)
	}
}

// TestCompose_NonASCIIHeaders: a subject and recipient names outside
// ASCII travel as RFC 2047 encoded words and decode to themselves; a
// subject's line breaks fold into spaces.
func TestCompose_NonASCIIHeaders(t *testing.T) {
	m, c := capture(t, Config{From: "商店 <shop@shop.example>"})
	subject := "你好，世界 — your order №42 has shipped and is on its way to you now"
	err := m.Send(t.Context(), Message{To: []string{"张三 <zhang@example.com>", `"Ann, B." <ann@example.com>`}, Subject: subject + "\r\nBcc: evil@example.com", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, raw := c.last(t)
	head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	if strings.Contains(head, "你") || strings.Contains(head, "张") || strings.Contains(head, "\nBcc:") {
		t.Fatalf("raw header:\n%s", head)
	}
	for line := range strings.Lines(head) {
		// RFC 2047: a line with encoded words is at most 76 characters.
		if l := strings.TrimRight(line, "\r\n"); len(l) > 76 && strings.Contains(l, "=?") {
			t.Fatalf("a header line of %d: %q", len(l), l)
		}
	}
	e := parseMail(t, raw)
	var dec mime.WordDecoder
	got, err := dec.DecodeHeader(e.get("Subject"))
	if err != nil || got != subject+" Bcc: evil@example.com" {
		t.Fatalf("subject %q: %v", got, err)
	}
	to, err := mail.ParseAddressList(e.get("To"))
	if err != nil || len(to) != 2 || to[0].Name != "张三" || to[0].Address != "zhang@example.com" || to[1].Name != "Ann, B." {
		t.Fatalf("To %q: %+v %v", e.get("To"), to, err)
	}
	from, err := mail.ParseAddress(e.get("From"))
	if err != nil || from.Name != "商店" {
		t.Fatalf("From %q: %v", e.get("From"), err)
	}
}

// TestCompose_Headers: extra fields go out, Reply-To among them replaces
// the configured one, and the mailer's own fields cannot be set.
func TestCompose_Headers(t *testing.T) {
	m, c := capture(t, Config{ReplyTo: "help@shop.example"})
	err := m.Send(t.Context(), Message{To: []string{"a@example.com"}, Text: "x", Headers: map[string]string{
		"list-unsubscribe": "<https://shop.example/unsubscribe?u=1>", "Reply-To": "orders@shop.example", "X-Order": "Bestellung №7",
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, raw := c.last(t)
	e := parseMail(t, raw)
	var dec mime.WordDecoder
	order, _ := dec.DecodeHeader(e.get("X-Order"))
	if e.get("List-Unsubscribe") != "<https://shop.example/unsubscribe?u=1>" || len(e.header["Reply-To"]) != 1 || e.get("Reply-To") != "orders@shop.example" || order != "Bestellung №7" {
		t.Fatalf("headers %v", e.header)
	}
	for _, k := range []string{"From", "to", "Cc", "BCC", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type", "content-transfer-encoding", "Content-Disposition"} {
		err := m.Send(t.Context(), Message{To: []string{"a@example.com"}, Text: "x", Headers: map[string]string{k: "v"}})
		if err == nil || !strings.Contains(err.Error(), "the mailer's own") {
			t.Errorf("Headers[%q]: %v", k, err)
		}
	}
	for _, h := range []map[string]string{{"X-A": "line\r\nBcc: evil@example.com"}, {"X A": "v"}, {"X:A": "v"}, {"": "v"}, {"X-Á": "v"}} {
		if err := m.Send(t.Context(), Message{To: []string{"a@example.com"}, Text: "x", Headers: h}); err == nil {
			t.Errorf("Headers %q accepted", h)
		}
	}
	if len(c.raws) != 1 {
		t.Fatalf("%d mails went out; only the first was valid", len(c.raws))
	}
}

// TestSend_Recipients: at least one, each an address; Bcc alone is
// enough; a bad one is named.
func TestSend_Recipients(t *testing.T) {
	m, c := capture(t, Config{})
	for _, msg := range []Message{
		{Text: "x"},
		{To: []string{"not an address"}, Text: "x"},
		{To: []string{"a@example.com", "b@example.com, c@example.com"}, Text: "x"},
		{To: []string{"a@example.com"}, Cc: []string{"<broken"}, Text: "x"},
		{To: []string{"a@example.com"}, Bcc: []string{""}, Text: "x"},
	} {
		if err := m.Send(t.Context(), msg); err == nil {
			t.Errorf("%+v accepted", msg)
		}
	}
	err := m.Send(t.Context(), Message{To: []string{"x"}, Text: "x"})
	if err == nil || !strings.Contains(err.Error(), `to "x" is not an address`) {
		t.Fatalf("the bad address is named: %v", err)
	}
	if err := m.Send(t.Context(), Message{Bcc: []string{"a@example.com"}, Text: "x"}); err != nil {
		t.Fatalf("Bcc alone: %v", err)
	}
	msg, _ := c.last(t)
	if rcpt, _ := recipients(&Message{To: []string{"A <a@example.com>"}, Cc: []string{"a@example.com"}, Bcc: []string{"b@example.com"}}); !slices.Equal(rcpt, []string{"a@example.com", "b@example.com"}) || msg.Bcc[0] != "a@example.com" {
		t.Fatalf("envelope %v", rcpt)
	}
}

// TestSend_OneBody: a Template or the body itself, never both, never
// neither; attachments need names.
func TestSend_OneBody(t *testing.T) {
	m, _ := capture(t, Config{})
	for msg, want := range map[*Message]string{
		{To: []string{"a@example.com"}}:                                                                                   "needs a Template, Text or HTML",
		{To: []string{"a@example.com"}, Template: "x", Text: "y"}:                                                         "give one or the other",
		{To: []string{"a@example.com"}, Template: "x"}:                                                                    "pass mail.Templates",
		{To: []string{"a@example.com"}, Text: "x", Attachments: []Attachment{{Content: []byte("y")}}}:                     "needs a name",
		{To: []string{"a@example.com"}, Text: "x", Attachments: []Attachment{{Name: "a b.png", Inline: true}}}:            "Content-ID",
		{To: []string{"a@example.com"}, Text: "x", Attachments: []Attachment{{Name: "a.bin", ContentType: "not a type"}}}: "content type",
	} {
		if err := m.Send(t.Context(), *msg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: want %q, got %v", *msg, want, err)
		}
	}
}
