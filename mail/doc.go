// Package mail is the mail battery: transactional mail — a sign-in code,
// a password reset, an order confirmation — sent with one value and the
// standard library only.
//
//	type Config struct {
//		chok.Base
//		Mail mail.Config `yaml:"mail"`
//	}
//
//	//go:embed templates
//	var templates embed.FS
//
//	m := mail.MustNew(cfg.Mail, mail.Templates(templates))
//	err := m.Send(ctx, mail.Message{To: []string{order.Email}, Template: "order-confirmed", Data: order})
//
// # Transports
//
// Config.Transport smtp sends through a relay (Resend, Postmark, Amazon
// SES and the like all offer one, and sign DKIM for you): each Send dials
// it, upgrades with STARTTLS or speaks TLS from the start, authenticates
// with AUTH PLAIN and delivers to every recipient, Bcc included, within
// Config.Timeout. dir writes every mail as an .eml file instead, for
// development and tests; log only logs it. The Sender option replaces the
// transport with a function, for an HTTP API or a test.
//
// # Templates
//
// With Templates, Message.Template names a template: {name}.html
// (html/template, sent as text/html), {name}.txt (text/template, sent as
// text/plain) or both, which go out together as multipart/alternative.
// _*.html and _*.txt files are partials, and {{define "subject"}} sets
// the subject.
//
// # Composition
//
// A mail is RFC 5322: From, Reply-To, To, Cc, Subject (encoded when not
// ASCII), Date, a Message-ID at From's domain and MIME-Version; the text
// in quoted-printable, attachments in base64; text and HTML in
// multipart/alternative, inside multipart/related with inline images,
// inside multipart/mixed with attachments. Bcc never appears.
//
// # Reliability
//
// Send is one attempt. To retry, enqueue the mail with jobs in the
// transaction that calls for it, and send it from the topic's Worker:
// docs/mail.md has the recipe, and the identity adapter.
package mail
