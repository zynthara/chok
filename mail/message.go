package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path"
	"slices"
	"strings"
	"time"
)

// Message is one mail. Give its body either as a Template, rendered with
// Data, or directly as Text, HTML or both; and at least one recipient.
type Message struct {
	// To, Cc and Bcc are the recipients, each "Name <addr>" or "addr".
	// Bcc recipients get the mail without appearing in it.
	To, Cc, Bcc []string
	// Subject is the subject line; with a Template, a non-empty Subject
	// replaces the template's.
	Subject string
	// Text and HTML are the body: plain text, HTML, or both, which go out
	// as multipart/alternative.
	Text, HTML string
	// Template names a template of the Templates option, without its
	// extension; Data is what it renders.
	Template string
	Data     any
	// Headers are extra header fields, such as List-Unsubscribe; one named
	// Reply-To replaces Config.ReplyTo. From, To, Cc, Bcc, Subject, Date,
	// Message-ID, MIME-Version and the Content-* fields are the mailer's:
	// setting one is an error.
	Headers map[string]string
	// Attachments travel with the mail.
	Attachments []Attachment
}

// Attachment is a file that travels with a mail.
type Attachment struct {
	// Name is the file name the recipient sees and, for an Inline
	// attachment, its Content-ID: the HTML shows it as <img src="cid:Name">.
	Name string
	// ContentType is the media type; empty, it is guessed from Name's
	// extension, and is application/octet-stream when that fails.
	ContentType string
	// Content is the file.
	Content []byte
	// Inline makes it part of the HTML body (multipart/related) rather than
	// an attachment of its own.
	Inline bool
}

// protected are the header fields the mailer writes itself, canonical.
var protected = []string{"Bcc", "Cc", "Date", "From", "Message-Id", "Mime-Version", "Subject", "To"}

// addresses parses each of list, naming the field of a bad one.
func addresses(field string, list []string) ([]*mail.Address, error) {
	out := make([]*mail.Address, 0, len(list))
	for _, s := range list {
		a, err := mail.ParseAddress(s)
		if err != nil {
			return nil, fmt.Errorf("mail: %s %q is not an address: %w", field, s, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// recipients are the envelope's: every address of To, Cc and Bcc, once.
func recipients(msg *Message) ([]string, error) {
	var out []string
	for _, f := range []struct {
		name string
		list []string
	}{{"to", msg.To}, {"cc", msg.Cc}, {"bcc", msg.Bcc}} {
		as, err := addresses(f.name, f.list)
		if err != nil {
			return nil, err
		}
		for _, a := range as {
			if !slices.Contains(out, a.Address) {
				out = append(out, a.Address)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("mail: a message needs a recipient in To, Cc or Bcc")
	}
	return out, nil
}

// compose writes msg, rendered, as an RFC 5322 message: the header, then a
// single text part, multipart/alternative for text and HTML, inside
// multipart/related with inline attachments, inside multipart/mixed with
// the others.
func (m *Mailer) compose(msg *Message) ([]byte, error) {
	if _, err := recipients(msg); err != nil {
		return nil, err
	}
	to, _ := addresses("to", msg.To)
	cc, _ := addresses("cc", msg.Cc)
	var h bytes.Buffer
	writeField(&h, "From", m.from.String())
	replyTo := ""
	if m.replyTo != nil {
		replyTo = m.replyTo.String()
	}
	extra, err := extraFields(msg.Headers)
	if err != nil {
		return nil, err
	}
	if v, ok := extra["Reply-To"]; ok {
		replyTo = v
		delete(extra, "Reply-To")
	}
	if replyTo != "" {
		writeField(&h, "Reply-To", replyTo)
	}
	if len(to) > 0 {
		writeField(&h, "To", joinAddresses(to))
	}
	if len(cc) > 0 {
		writeField(&h, "Cc", joinAddresses(cc))
	}
	writeField(&h, "Subject", encodeText(oneLine(msg.Subject)))
	writeField(&h, "Date", time.Now().UTC().Format(time.RFC1123Z))
	writeField(&h, "Message-ID", "<"+randomHex(16)+"@"+m.domain+">")
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		writeField(&h, k, extra[k])
	}
	writeField(&h, "MIME-Version", "1.0")

	body, err := bodyOf(msg)
	if err != nil {
		return nil, err
	}
	for _, k := range slices.Sorted(maps.Keys(body.header)) {
		writeField(&h, k, body.header.Get(k))
	}
	h.WriteString("\r\n")
	h.Write(body.body)
	return h.Bytes(), nil
}

// extraFields checks Headers: a field name, a value on one line, none of
// the mailer's own fields. Values outside ASCII are encoded.
func extraFields(in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !validFieldName(k) {
			return nil, fmt.Errorf("mail: header %q is not a field name", k)
		}
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if slices.Contains(protected, ck) || strings.HasPrefix(ck, "Content-") {
			return nil, fmt.Errorf("mail: header %s is the mailer's own; Headers cannot set it", k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("mail: header %s: the value spans lines", k)
		}
		if _, dup := out[ck]; dup {
			return nil, fmt.Errorf("mail: header %s is given twice", ck)
		}
		out[ck] = encodeText(v)
	}
	return out, nil
}

// validFieldName reports printable ASCII without a colon or space, which
// is what RFC 5322 allows a field name.
func validFieldName(k string) bool {
	if k == "" {
		return false
	}
	for i := range len(k) {
		if c := k[i]; c <= ' ' || c > '~' || c == ':' {
			return false
		}
	}
	return true
}

// part is one MIME entity: its header and its encoded body.
type part struct {
	header textproto.MIMEHeader
	body   []byte
}

// bodyOf builds msg's MIME tree.
func bodyOf(msg *Message) (part, error) {
	var alts []part
	if msg.Text != "" {
		alts = append(alts, textPart("text/plain", msg.Text))
	}
	if msg.HTML != "" {
		alts = append(alts, textPart("text/html", msg.HTML))
	}
	var body part
	switch len(alts) {
	case 0:
		return part{}, errors.New("mail: a message needs a Template, Text or HTML")
	case 1:
		body = alts[0]
	default:
		body = multipartOf("alternative", alts)
	}
	var inline, attached []part
	for _, a := range msg.Attachments {
		p, err := attachmentPart(a)
		if err != nil {
			return part{}, err
		}
		if a.Inline {
			inline = append(inline, p)
		} else {
			attached = append(attached, p)
		}
	}
	if len(inline) > 0 {
		body = multipartOf("related", append([]part{body}, inline...))
	}
	if len(attached) > 0 {
		body = multipartOf("mixed", append([]part{body}, attached...))
	}
	return body, nil
}

// textPart is text as UTF-8, quoted-printable.
func textPart(mediaType, text string) part {
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(strings.ReplaceAll(text, "\r\n", "\n")))
	_ = w.Close()
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", mediaType+"; charset=utf-8")
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	return part{header: h, body: b.Bytes()}
}

// attachmentPart is a, base64.
func attachmentPart(a Attachment) (part, error) {
	if a.Name == "" || strings.ContainsFunc(a.Name, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return part{}, fmt.Errorf("mail: attachment %q needs a name on one line", a.Name)
	}
	if a.Inline && strings.ContainsAny(a.Name, " <>\"\\") {
		return part{}, fmt.Errorf("mail: inline attachment %q: its name is its Content-ID, which takes no space, quote or angle bracket", a.Name)
	}
	ct := a.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(a.Name))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return part{}, fmt.Errorf("mail: attachment %q: content type %q: %w", a.Name, ct, err)
	}
	params["name"] = a.Name
	disposition := "attachment"
	if a.Inline {
		disposition = "inline"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.Name}))
	h.Set("Content-Transfer-Encoding", "base64")
	if a.Inline {
		h.Set("Content-Id", "<"+a.Name+">")
	}
	return part{header: h, body: base64Lines(a.Content)}, nil
}

// base64Lines is b in base64, in lines of 76 characters.
func base64Lines(b []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(b)
	var out bytes.Buffer
	for len(enc) > 76 {
		out.WriteString(enc[:76])
		out.WriteString("\r\n")
		enc = enc[76:]
	}
	out.WriteString(enc)
	out.WriteString("\r\n")
	return out.Bytes()
}

// multipartOf wraps parts in a multipart/<subtype> entity.
func multipartOf(subtype string, parts []part) part {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	// 28 hex digits: random enough never to occur in a part, short enough
	// for the Content-Type line to fit 76 characters.
	_ = w.SetBoundary(randomHex(14))
	for _, p := range parts {
		pw, _ := w.CreatePart(p.header) // writes to a buffer: no error
		_, _ = pw.Write(p.body)
	}
	_ = w.Close()
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", "multipart/"+subtype+"; boundary="+w.Boundary())
	return part{header: h, body: b.Bytes()}
}

// foldWidth is the longest header line writeField makes where it can:
// RFC 2047's limit for a line with encoded words, under RFC 5322's 78.
const foldWidth = 76

// writeField writes one header field, folded at spaces into lines of at
// most foldWidth characters wherever a word allows it; the first word
// may start on a continuation line of its own.
func writeField(b *bytes.Buffer, name, value string) {
	b.WriteString(name + ":")
	width := len(name) + 1
	for word := range strings.SplitSeq(value, " ") {
		if width+1+len(word) > foldWidth && width > 1 {
			b.WriteString("\r\n")
			width = 0
		}
		b.WriteString(" " + word)
		width += 1 + len(word)
	}
	b.WriteString("\r\n")
}

func joinAddresses(as []*mail.Address) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// oneLine collapses s's runs of white space, line breaks included, into
// single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// encodeText is s as RFC 2047 encoded words when it is not plain ASCII.
func encodeText(s string) string { return mime.QEncoding.Encode("utf-8", s) }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand never fails on supported platforms
	return hex.EncodeToString(b)
}
