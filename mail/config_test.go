package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/internal/conf"
)

func TestConfig_Validate(t *testing.T) {
	ok := Config{From: testFrom}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a sender is all a config needs: %v", err)
	}
	for name, c := range map[string]struct {
		cfg  Config
		want string
	}{
		"no sender":           {Config{}, "from is required"},
		"a bad sender":        {Config{From: "shop"}, `from "shop" is not an address`},
		"a bad reply-to":      {Config{From: testFrom, ReplyTo: "help@"}, "reply_to"},
		"a negative timeout":  {Config{From: testFrom, Timeout: -time.Second}, "timeout must not be negative"},
		"a port out of range": {Config{From: testFrom, SMTP: SMTPConfig{Port: 70000}}, "smtp.port 70000"},
	} {
		if err := c.cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}

// TestConfig_LoadsWithDefaults: through chok.Load the section has its
// defaults, the password comes from the environment and stays masked.
func TestConfig_LoadsWithDefaults(t *testing.T) {
	type appConfig struct {
		chok.Base
		Mail Config `yaml:"mail"`
	}
	t.Setenv("MLT_MAIL_FROM", "Shop <no-reply@shop.example>")
	t.Setenv("MLT_MAIL_SMTP_HOST", "smtp.example.com")
	t.Setenv("MLT_MAIL_SMTP_PASSWORD", "relay-secret-value")
	cfg, err := chok.Load[appConfig]("mlt", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Mail
	if c.Transport != "smtp" || c.Timeout != 15*time.Second || c.SMTP.Port != 587 || c.SMTP.TLS != "starttls" || c.SMTP.Password != "relay-secret-value" {
		t.Fatalf("defaults: %+v", c)
	}
	out, err := conf.Dump(cfg)
	if err != nil || strings.Contains(string(out), "relay-secret-value") || !strings.Contains(string(out), conf.Masked) {
		t.Fatalf("the password is a secret:\n%s", out)
	}
	for _, bad := range []struct{ key, value, want string }{
		{"MLT_MAIL_TRANSPORT", "carrier-pigeon", "transport"},
		{"MLT_MAIL_SMTP_TLS", "ssl", "tls"},
		{"MLT_MAIL_FROM", "", "from is required"},
	} {
		t.Run(bad.key, func(t *testing.T) {
			t.Setenv(bad.key, bad.value)
			if _, err := chok.Load[appConfig]("mlt", chok.Args(nil)); err == nil || !strings.Contains(err.Error(), bad.want) {
				t.Errorf("%s=%q: want %q, got %v", bad.key, bad.value, bad.want, err)
			}
		})
	}
}

// TestNew_DefaultsAndChecks: a Config built in code gets its defaults;
// New checks what the transport needs, unless a Sender replaces it.
func TestNew_DefaultsAndChecks(t *testing.T) {
	m, err := New(Config{From: testFrom, SMTP: SMTPConfig{Host: "smtp.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if c := m.cfg; c.Transport != "smtp" || c.Timeout != 15*time.Second || c.SMTP.Port != 587 || c.SMTP.TLS != "starttls" {
		t.Fatalf("defaults in code: %+v", c)
	}
	if m.Name() != "mail" || m.Health(t.Context()) != nil || m.domain != "shop.example" {
		t.Fatal("the component")
	}
	dir := filepath.Join(t.TempDir(), "a", "b")
	if _, err := New(Config{From: testFrom, Transport: "dir", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("New creates the directory: %v", err)
	}
	for name, c := range map[string]struct {
		cfg  Config
		opts []Option
		want string
	}{
		"no host":                {Config{From: testFrom}, nil, "smtp.host is required"},
		"cleartext to a remote":  {Config{From: testFrom, SMTP: SMTPConfig{Host: "smtp.example.com", TLS: "none"}}, nil, "only a loopback host"},
		"dir without a dir":      {Config{From: testFrom, Transport: "dir"}, nil, "dir is required"},
		"an unknown transport":   {Config{From: testFrom, Transport: "fax"}, nil, "transport"},
		"an unknown tls":         {Config{From: testFrom, SMTP: SMTPConfig{Host: "h", TLS: "ssl"}}, nil, "tls"},
		"no sender":              {Config{Transport: "log"}, nil, "from is required"},
		"a nil Sender":           {Config{From: testFrom, Transport: "log"}, []Option{Sender(nil)}, "Sender(nil)"},
		"a nil option":           {Config{From: testFrom, Transport: "log"}, []Option{nil}, "nil Option"},
		"a dir that is a file":   {Config{From: testFrom, Transport: "dir", Dir: filepath.Join(dir, "f", "g")}, nil, "dir"},
		"cleartext to localhost": {Config{From: testFrom, SMTP: SMTPConfig{Host: "localhost", TLS: "none"}}, nil, ""},
		"cleartext to ::1":       {Config{From: testFrom, SMTP: SMTPConfig{Host: "::1", TLS: "none"}}, nil, ""},
		"a Sender, no host":      {Config{From: testFrom}, []Option{Sender(func(context.Context, *Message, []byte) error { return nil })}, ""},
	} {
		if name == "a dir that is a file" {
			if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := New(c.cfg, c.opts...)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustNew panics on a bad config")
		}
	}()
	MustNew(Config{})
}

// TestSend_TimeoutAndErrors: the transport gets Config.Timeout as its
// deadline, and its error comes back wrapped.
func TestSend_TimeoutAndErrors(t *testing.T) {
	boom := errors.New("the API said no")
	var deadline time.Time
	m, err := New(Config{From: testFrom, Timeout: 3 * time.Second}, Sender(func(ctx context.Context, _ *Message, _ []byte) error {
		deadline, _ = ctx.Deadline()
		return boom
	}))
	if err != nil {
		t.Fatal(err)
	}
	err = m.Send(t.Context(), Message{To: []string{"ada@example.com"}, Subject: "Hi", Text: "x"})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), `send "Hi" to ada@example.com`) {
		t.Fatalf("the transport's error: %v", err)
	}
	if left := time.Until(deadline); left <= 0 || left > 3*time.Second {
		t.Fatalf("deadline in %s", left)
	}
}
