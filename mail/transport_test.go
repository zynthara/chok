package mail

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDir_WritesEmlFiles: transport dir writes each mail, whole, to its
// own {unix ns}-{random}.eml, readable by its owner only, and logs where.
func TestDir_WritesEmlFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outbox")
	var logs syncBuffer
	m, err := New(Config{From: testFrom, Transport: "dir", Dir: dir}, Logger(debugLogger(&logs)))
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"ada@example.com", "bob@example.com"} {
		if err := m.Send(t.Context(), Message{To: []string{to}, Subject: "Code 123456", Text: "Your code is 123456."}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("%d files: %v", len(entries), err)
	}
	name := regexp.MustCompile(`^\d{19}-[0-9a-f]{8}\.eml$`)
	for i, e := range entries {
		if !name.MatchString(e.Name()) {
			t.Fatalf("file name %s", e.Name())
		}
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v", e.Name(), info.Mode())
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		mail := parseMail(t, raw)
		if want := []string{"<ada@example.com>", "<bob@example.com>"}[i]; mail.get("To") != want || mail.body != "Your code is 123456." {
			t.Fatalf("%s: To %q, body %q", e.Name(), mail.get("To"), mail.body)
		}
		if !strings.Contains(logs.String(), "path="+filepath.Join(dir, e.Name())) {
			t.Fatalf("the log names %s:\n%s", e.Name(), logs.String())
		}
	}
	if !strings.Contains(logs.String(), "level=INFO msg=\"mail: written\"") || !strings.Contains(logs.String(), `subject="Code 123456"`) {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

// TestLog_LogsOnly: transport log logs the mail at Info — recipients,
// subject, size — and its body at Debug, and sends nothing.
func TestLog_LogsOnly(t *testing.T) {
	var logs syncBuffer
	m, err := New(Config{From: testFrom, Transport: "log"}, Logger(debugLogger(&logs)))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(t.Context(), Message{To: []string{"ada@example.com", "bob@example.com"}, Subject: "Hello", Text: "plain body", HTML: "<b>rich body</b>"}); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	info := regexp.MustCompile(`level=INFO msg="mail: logged, not sent" to="ada@example.com, bob@example.com" subject=Hello bytes=\d+`)
	if !info.MatchString(out) || !strings.Contains(out, `level=DEBUG msg="mail: body" text="plain body" html="<b>rich body</b>"`) {
		t.Fatalf("logs:\n%s", out)
	}
}

// TestSender_ReplacesTheTransport: with a Sender, Transport and its
// settings do not matter, and the Sender gets the rendered message and
// its RFC 5322 text.
func TestSender_ReplacesTheTransport(t *testing.T) {
	m, c := capture(t, Config{Transport: "dir"}) // no dir: the Sender replaces the transport
	if err := m.Send(t.Context(), Message{To: []string{"ada@example.com"}, Subject: "S", HTML: "<p>x</p>"}); err != nil {
		t.Fatal(err)
	}
	msg, raw := c.last(t)
	if msg.Subject != "S" || msg.HTML != "<p>x</p>" || !strings.HasPrefix(string(raw), "From: \"Shop\" <no-reply@shop.example>\r\n") {
		t.Fatalf("%+v\n%s", msg, raw)
	}
}
