package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestForgotPassword_MailsTheResetLink: with auth.reset_url the
// password-reset Mail carries the page's link with the token.
func TestForgotPassword_MailsTheResetLink(t *testing.T) {
	for base, want := range map[string]string{
		"https://app.example/reset":         "https://app.example/reset?token=",
		"https://app.example/reset?lang=en": "https://app.example/reset?lang=en&token=",
	} {
		sender := newMockSender()
		c := serve(t, newIdentity(t, Config{ResetURL: base}, Sender(sender.send)))
		register(t, c, "alice@test.com", "password123")
		if code := c.PostJSON("/auth/forgot-password", map[string]string{"email": "alice@test.com"}, nil); code != http.StatusNoContent {
			t.Fatalf("forgot = %d", code)
		}
		if !sender.wait(5 * time.Second) {
			t.Fatal("no mail")
		}
		if m := sender.last(); m.Kind != "password-reset" || m.Token == "" || m.Link != want+m.Token {
			t.Fatalf("with reset_url %s: %+v", base, m)
		}
	}
}

// TestSender_GetsAddressTemplateAndMail: the Sender is called with the
// address, the template's name — the Mail's Kind — and the Mail itself as
// the template's data, for both kinds of mail.
func TestSender_GetsAddressTemplateAndMail(t *testing.T) {
	sender := newMockSender()
	c := serve(t, newIdentity(t, Config{Passwordless: PasswordlessConfig{Enabled: true}}, Sender(sender.send)))
	register(t, c, "alice@test.com", "password123")
	if code := c.PostJSON("/auth/forgot-password", map[string]string{"email": "alice@test.com"}, nil); code != http.StatusNoContent {
		t.Fatalf("forgot = %d", code)
	}
	if !sender.wait(5 * time.Second) {
		t.Fatal("no reset mail")
	}
	if code := c.PostJSON("/auth/passwordless/start", map[string]string{"email": "bob@test.com"}, nil); code != http.StatusAccepted {
		t.Fatalf("start = %d", code)
	}
	if !sender.wait(5 * time.Second) {
		t.Fatal("no code mail")
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.calls) != 2 {
		t.Fatalf("calls: %+v", sender.calls)
	}
	for i, want := range []struct{ to, template string }{{"alice@test.com", "password-reset"}, {"bob@test.com", "login-code"}} {
		call := sender.calls[i]
		m, ok := call.data.(Mail)
		if call.to != want.to || call.template != want.template || !ok || m.To != want.to || m.Kind != want.template || m.Token == "" || m.ExpiresAt.IsZero() {
			t.Errorf("call %d: %q %q %#v", i, call.to, call.template, call.data)
		}
	}
	if m := sender.calls[1].data.(Mail); len(m.Code) != 6 {
		t.Errorf("the login code is in the data: %+v", m)
	}
}

// TestMail_FailureIsAWarning: a Sender that fails costs the response
// nothing; the failure is a warning in the log, with the address masked.
func TestMail_FailureIsAWarning(t *testing.T) {
	sender := newMockSender()
	sender.err = errors.New("the relay is down")
	idn := newIdentity(t, Config{}, Sender(sender.send))
	h, logs := loggedApp(t, idn)
	if _, err := idn.Users().Create(t.Context(), "alice@test.com", "password123", "Alice"); err != nil {
		t.Fatal(err)
	}
	if rec := serveJSON(t, h, http.MethodPost, "/auth/forgot-password", map[string]string{"email": "alice@test.com"}, "X-Request-Id", "req-forgot"); rec.Code != http.StatusNoContent {
		t.Fatalf("forgot = %d %s", rec.Code, rec.Body)
	}
	eventually(t, "the warning", func() bool { return strings.Contains(logs.String(), "identity: mail not sent") })
	line := logs.String()
	for _, want := range []string{"level=WARN", "kind=password-reset", "to=a***e@t**t.com", `error="the relay is down"`, "request_id=req-forgot"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the warning lacks %s:\n%s", want, line)
		}
	}
	if strings.Contains(line, "alice@test.com") {
		t.Fatalf("the address is masked:\n%s", line)
	}
}

// blockingSender holds every Send until released or canceled, and
// reports how each ended.
type blockingSender struct {
	started chan Mail
	release chan struct{}
	ended   chan error
}

func newBlockingSender() *blockingSender {
	return &blockingSender{started: make(chan Mail, 8), release: make(chan struct{}), ended: make(chan error, 8)}
}

func (s *blockingSender) send(ctx context.Context, _, _ string, data any) error {
	s.started <- data.(Mail)
	select {
	case <-s.release:
		s.ended <- nil
		return nil
	case <-ctx.Done():
		s.ended <- ctx.Err()
		return ctx.Err()
	}
}

// TestClose_WaitsForTheMailsInFlight: Close waits for a mail being sent,
// within its context; when the context ends first, Close reports it and
// the send is canceled. After Close no mail starts.
func TestClose_WaitsForTheMailsInFlight(t *testing.T) {
	forgot := func(t *testing.T, sender *blockingSender) (*Identity, http.Handler, *syncBuffer) {
		idn := newIdentity(t, Config{}, Sender(sender.send))
		h, logs := loggedApp(t, idn)
		if _, err := idn.Users().Create(t.Context(), "alice@test.com", "password123", "Alice"); err != nil {
			t.Fatal(err)
		}
		if rec := serveJSON(t, h, http.MethodPost, "/auth/forgot-password", map[string]string{"email": "alice@test.com"}); rec.Code != http.StatusNoContent {
			t.Fatalf("forgot = %d", rec.Code)
		}
		select {
		case <-sender.started:
		case <-time.After(5 * time.Second):
			t.Fatal("the send did not start")
		}
		return idn, h, logs
	}

	t.Run("it waits", func(t *testing.T) {
		sender := newBlockingSender()
		idn, _, _ := forgot(t, sender)
		closed := make(chan error, 1)
		go func() { closed <- idn.Close(t.Context()) }()
		select {
		case err := <-closed:
			t.Fatalf("Close returned while a mail was in flight: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(sender.release)
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if err := <-sender.ended; err != nil {
			t.Fatalf("the send ended with %v", err)
		}
	})

	t.Run("within its context", func(t *testing.T) {
		sender := newBlockingSender()
		idn, h, logs := forgot(t, sender)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		err := idn.Close(ctx)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "mails in flight") {
			t.Fatalf("Close: %v", err)
		}
		select {
		case err := <-sender.ended:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the send ended with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the send was not canceled")
		}
		// Closed, the identity sends no more.
		if rec := serveJSON(t, h, http.MethodPost, "/auth/forgot-password", map[string]string{"email": "alice@test.com"}); rec.Code != http.StatusNoContent {
			t.Fatalf("forgot after Close = %d", rec.Code)
		}
		eventually(t, "the warning", func() bool { return strings.Contains(logs.String(), "identity: closed; mail not sent") })
		if len(sender.started) != 0 {
			t.Fatal("a mail started after Close")
		}
	})
}
