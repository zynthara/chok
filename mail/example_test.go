package mail_test

import (
	"context"
	"fmt"
	"testing/fstest"

	"github.com/zynthara/chok/v3/mail"
)

// A mailer with templates; here a Sender stands in for the relay and
// prints what would go out.
func Example() {
	templates := fstest.MapFS{ // usually an embed.FS of a templates directory
		"order-confirmed.html": {Data: []byte(`{{define "subject"}}Order {{.ID}} confirmed{{end}}<p>Thanks, {{.Name}}!</p>`)},
		"order-confirmed.txt":  {Data: []byte(`Thanks, {{.Name}}!`)},
	}
	m, err := mail.New(mail.Config{From: "Shop <no-reply@shop.example>"},
		mail.Templates(templates),
		mail.Sender(func(_ context.Context, msg *mail.Message, raw []byte) error {
			fmt.Printf("%s, to %v\n%s\n%s\n", msg.Subject, msg.To, msg.Text, msg.HTML)
			return nil
		}))
	if err != nil {
		fmt.Println(err)
		return
	}
	order := struct {
		ID   int
		Name string
	}{42, "Ada"}
	if err := m.Send(context.Background(), mail.Message{To: []string{"Ada <ada@example.com>"}, Template: "order-confirmed", Data: order}); err != nil {
		fmt.Println(err)
	}
	// Output:
	// Order 42 confirmed, to [Ada <ada@example.com>]
	// Thanks, Ada!
	// <p>Thanks, Ada!</p>
}

// An attachment, and an image the HTML shows inline.
func ExampleAttachment() {
	logo, invoice := []byte("…png…"), []byte("…pdf…")
	msg := mail.Message{
		To:      []string{"ada@example.com"},
		Subject: "Your invoice",
		HTML:    `<img src="cid:logo.png" alt="Shop"><p>Your invoice is attached.</p>`,
		Attachments: []mail.Attachment{
			{Name: "logo.png", Content: logo, Inline: true},
			{Name: "invoice-42.pdf", Content: invoice}, // application/pdf, from the extension
		},
	}
	_ = msg // m.Send(ctx, msg)
}
