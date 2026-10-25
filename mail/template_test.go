package mail

import (
	"strings"
	"testing"
	"testing/fstest"
)

type order struct {
	ID    int
	Name  string
	Items []string
}

// TestTemplates_SubjectAndAlternative: a template's subject comes from its
// define, {name}.txt rides along as the plain-text alternative, and the
// Sender sees the message as rendered.
func TestTemplates_SubjectAndAlternative(t *testing.T) {
	fsys := fstest.MapFS{
		"order-confirmed.html": {Data: []byte(`{{define "subject"}}Order #{{.ID}} & more{{end}}<p>Thanks, {{.Name}}!</p>`)},
		"order-confirmed.txt":  {Data: []byte(`{{define "subject"}}Order #{{.ID}}, confirmed{{end}}Thanks, {{.Name}}!`)},
	}
	m, c := capture(t, Config{}, Templates(fsys))
	if err := m.Send(t.Context(), Message{To: []string{"ada@example.com"}, Template: "order-confirmed", Data: order{ID: 42, Name: "Ada <3"}}); err != nil {
		t.Fatal(err)
	}
	msg, raw := c.last(t)
	// The text template's subject wins: it is plain text already.
	if msg.Subject != "Order #42, confirmed" || msg.Text != "Thanks, Ada <3!" || msg.HTML != "<p>Thanks, Ada &lt;3!</p>" || msg.Template != "order-confirmed" {
		t.Fatalf("rendered %+v", msg)
	}
	e := parseMail(t, raw)
	if e.get("Subject") != "Order #42, confirmed" || e.mediaType != "multipart/alternative" || e.parts[0].body != "Thanks, Ada <3!" || e.parts[1].body != "<p>Thanks, Ada &lt;3!</p>" {
		t.Fatalf("%v %s", e.header, e.mediaType)
	}
	// Message.Subject replaces the template's.
	if err := m.Send(t.Context(), Message{To: []string{"ada@example.com"}, Subject: "Mine", Template: "order-confirmed", Data: order{ID: 1}}); err != nil {
		t.Fatal(err)
	}
	if msg, _ := c.last(t); msg.Subject != "Mine" {
		t.Fatalf("subject %q", msg.Subject)
	}
}

// TestTemplates_TextIsOptional: without {name}.txt the mail is the HTML
// alone, and an HTML subject is unescaped to plain text.
func TestTemplates_TextIsOptional(t *testing.T) {
	fsys := fstest.MapFS{"welcome.html": {Data: []byte(`{{define "subject"}}  Welcome,
	{{.}} & co  {{end}}<h1>Hi {{.}}</h1>`)}}
	m, c := capture(t, Config{}, Templates(fsys))
	if err := m.Send(t.Context(), Message{To: []string{"ada@example.com"}, Template: "welcome", Data: "Ada & Bob"}); err != nil {
		t.Fatal(err)
	}
	msg, raw := c.last(t)
	if msg.Text != "" || msg.HTML != "<h1>Hi Ada &amp; Bob</h1>" {
		t.Fatalf("rendered %+v", msg)
	}
	e := parseMail(t, raw)
	if e.mediaType != "text/html" || e.get("Subject") != "Welcome, Ada & Bob & co" {
		t.Fatalf("%s, subject %q", e.mediaType, e.get("Subject"))
	}
}

// TestTemplates_Partials: _*.html and _*.txt files are the partials of
// every template of their kind, and templates are found by file name in
// any directory.
func TestTemplates_Partials(t *testing.T) {
	fsys := fstest.MapFS{
		"templates/_layout.html":      {Data: []byte(`{{define "layout"}}<main>{{template "content" .}}</main>{{template "_footer.html"}}{{end}}`)},
		"templates/_footer.html":      {Data: []byte(`<footer>Shop</footer>`)},
		"templates/_sign.txt":         {Data: []byte(`{{define "sign"}}-- Shop{{end}}`)},
		"templates/mail/shipped.html": {Data: []byte(`{{define "content"}}Shipped: {{range .Items}}<b>{{.}}</b>{{end}}{{end}}{{template "layout" .}}`)},
		"templates/mail/shipped.txt":  {Data: []byte("Shipped: {{range .Items}}{{.}} {{end}}\n{{template \"sign\"}}")},
		"templates/mail/receipt.html": {Data: []byte(`{{define "content"}}Receipt {{.ID}}{{end}}{{template "layout" .}}`)},
		"templates/mail/logo.png":     {Data: []byte("not a template")},
		"templates/mail/notes/readme": {Data: []byte("ignored")},
	}
	m, c := capture(t, Config{}, Templates(fsys))
	if err := m.Send(t.Context(), Message{To: []string{"ada@example.com"}, Template: "shipped", Data: order{Items: []string{"tea", "cups"}}}); err != nil {
		t.Fatal(err)
	}
	msg, _ := c.last(t)
	if msg.HTML != "<main>Shipped: <b>tea</b><b>cups</b></main><footer>Shop</footer>" || msg.Text != "Shipped: tea cups \n-- Shop" {
		t.Fatalf("rendered %q / %q", msg.HTML, msg.Text)
	}
	if err := m.Send(t.Context(), Message{To: []string{"ada@example.com"}, Template: "receipt", Data: order{ID: 7}}); err != nil {
		t.Fatal(err)
	}
	if msg, _ := c.last(t); msg.HTML != "<main>Receipt 7</main><footer>Shop</footer>" || msg.Subject != "" {
		t.Fatalf("each template has its own content block: %q", msg.HTML)
	}
}

// TestTemplates_Errors: what New refuses and what Send does.
func TestTemplates_Errors(t *testing.T) {
	good := fstest.MapFS{"a.html": {Data: []byte("a")}}
	for name, c := range map[string]struct {
		opts []Option
		want string
	}{
		"given twice":       {[]Option{Templates(good), Templates(good)}, "Templates is given twice"},
		"nil":               {[]Option{Templates(nil)}, "Templates(nil)"},
		"no template":       {[]Option{Templates(fstest.MapFS{"_partial.html": {Data: []byte("p")}, "x.css": {}})}, "no {name}.html template"},
		"a text alone":      {[]Option{Templates(fstest.MapFS{"a.html": {Data: []byte("a")}, "b.txt": {Data: []byte("b")}})}, "b.txt has no b.html"},
		"a parse error":     {[]Option{Templates(fstest.MapFS{"a.html": {Data: []byte("{{.Name")}})}, "a.html"},
		"a text parse":      {[]Option{Templates(fstest.MapFS{"a.html": {Data: []byte("a")}, "a.txt": {Data: []byte("{{end}}")}})}, "a.txt"},
		"a partial's error": {[]Option{Templates(fstest.MapFS{"a.html": {Data: []byte("a")}, "_p.html": {Data: []byte("{{if}}")}})}, "_p.html"},
		"one name twice":    {[]Option{Templates(fstest.MapFS{"x/a.html": {Data: []byte("a")}, "y/a.html": {Data: []byte("a")}})}, "share a name"},
		"an undefined call": {[]Option{Templates(fstest.MapFS{"a.html": {Data: []byte(`{{template "nope"}}`)}})}, ""},
	} {
		_, err := New(Config{From: testFrom, Transport: "log"}, c.opts...)
		if c.want == "" {
			// html/template resolves template calls when it first runs.
			if err != nil {
				t.Errorf("%s: New: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	m, _ := capture(t, Config{}, Templates(fstest.MapFS{
		"a.html":   {Data: []byte("a")},
		"b.html":   {Data: []byte(`{{.Missing}}`)},
		"bad.html": {Data: []byte(`{{template "nope"}}`)},
	}))
	for tpl, want := range map[string]string{"zz": `no template "zz" among a, b, bad`, "b": "template b:", "bad": "template bad:"} {
		err := m.Send(t.Context(), Message{To: []string{"a@example.com"}, Template: tpl, Data: struct{}{}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Template %q: want %q, got %v", tpl, want, err)
		}
	}
}
