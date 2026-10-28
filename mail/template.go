package mail

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	htmltemplate "html/template"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	texttemplate "text/template"
)

// templates are the Templates option's templates, parsed, by name.
type templates struct {
	byName map[string]*page
}

// page is one template: {name}.html, {name}.txt or both, each parsed
// with the partials of its kind.
type page struct {
	html *htmltemplate.Template // nil without {name}.html
	text *texttemplate.Template // nil without {name}.txt
}

// source is one template file found in the file system.
type source struct {
	path, body string
}

// parseTemplates finds every .html and .txt file in fsys, at any depth,
// and parses each template with the partials of its kind.
func parseTemplates(fsys fs.FS) (*templates, error) {
	pages := map[string]map[string]source{} // name → ext → file
	partials := map[string][]source{}       // ext → files
	seen := map[string]string{}             // base name → path
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		base, ext := path.Base(p), path.Ext(p)
		if ext != ".html" && ext != ".txt" {
			return nil
		}
		if other, dup := seen[base]; dup {
			return fmt.Errorf("%s and %s share a name: templates are named by file, wherever they sit", other, p)
		}
		seen[base] = p
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		src := source{path: p, body: string(b)}
		if strings.HasPrefix(base, "_") {
			partials[ext] = append(partials[ext], src)
			return nil
		}
		name := strings.TrimSuffix(base, ext)
		if pages[name] == nil {
			pages[name] = map[string]source{}
		}
		pages[name][ext] = src
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("mail: Templates: %w", err)
	}
	if len(pages) == 0 {
		return nil, errors.New("mail: Templates: the file system has no template: {name}.html, {name}.txt or both")
	}
	t := &templates{byName: map[string]*page{}}
	for _, name := range slices.Sorted(maps.Keys(pages)) {
		files := pages[name]
		p := &page{}
		if htmlSrc, ok := files[".html"]; ok {
			root := htmltemplate.New(path.Base(htmlSrc.path))
			for _, s := range partials[".html"] {
				if _, err := root.New(path.Base(s.path)).Parse(s.body); err != nil {
					return nil, fmt.Errorf("mail: template %s: %w", s.path, err)
				}
			}
			if _, err := root.Parse(htmlSrc.body); err != nil {
				return nil, fmt.Errorf("mail: template %s: %w", htmlSrc.path, err)
			}
			p.html = root
		}
		if txtSrc, ok := files[".txt"]; ok {
			troot := texttemplate.New(path.Base(txtSrc.path))
			for _, s := range partials[".txt"] {
				if _, err := troot.New(path.Base(s.path)).Parse(s.body); err != nil {
					return nil, fmt.Errorf("mail: template %s: %w", s.path, err)
				}
			}
			if _, err := troot.Parse(txtSrc.body); err != nil {
				return nil, fmt.Errorf("mail: template %s: %w", txtSrc.path, err)
			}
			p.text = troot
		}
		t.byName[name] = p
	}
	return t, nil
}

// render executes the page with data: its HTML and its text, the ones it
// has, and its subject — the text template's {{define "subject"}} first,
// else the HTML one's, unescaped.
func (p *page) render(data any) (subject, text, htmlBody string, err error) {
	var b bytes.Buffer
	if p.html != nil {
		if err := p.html.Execute(&b, data); err != nil {
			return "", "", "", err
		}
		htmlBody = b.String()
	}
	if p.text != nil {
		b.Reset()
		if err := p.text.Execute(&b, data); err != nil {
			return "", "", "", err
		}
		text = b.String()
		if p.text.Lookup("subject") != nil {
			b.Reset()
			if err := p.text.ExecuteTemplate(&b, "subject", data); err != nil {
				return "", "", "", err
			}
			return b.String(), text, htmlBody, nil
		}
	}
	if p.html != nil && p.html.Lookup("subject") != nil {
		b.Reset()
		if err := p.html.ExecuteTemplate(&b, "subject", data); err != nil {
			return "", "", "", err
		}
		subject = html.UnescapeString(b.String())
	}
	return subject, text, htmlBody, nil
}

// render fills in msg's body from its template, and checks that it has
// one way to a body: a Template, or Text and HTML.
func (m *Mailer) render(msg Message) (*Message, error) {
	out := msg
	if msg.Template == "" {
		if msg.Text == "" && msg.HTML == "" {
			return nil, errors.New("mail: a message needs a Template, Text or HTML")
		}
		return &out, nil
	}
	if msg.Text != "" || msg.HTML != "" {
		return nil, fmt.Errorf("mail: message with Template %q and Text or HTML: give one or the other", msg.Template)
	}
	if m.templates == nil {
		return nil, fmt.Errorf("mail: no template %q: the mailer has none, pass mail.Templates to New", msg.Template)
	}
	p, ok := m.templates.byName[msg.Template]
	if !ok {
		return nil, fmt.Errorf("mail: no template %q among %s", msg.Template, strings.Join(slices.Sorted(maps.Keys(m.templates.byName)), ", "))
	}
	subject, text, htmlBody, err := p.render(msg.Data)
	if err != nil {
		return nil, fmt.Errorf("mail: template %s: %w", msg.Template, err)
	}
	out.Text, out.HTML = text, htmlBody
	if out.Subject == "" {
		out.Subject = subject
	}
	return &out, nil
}
