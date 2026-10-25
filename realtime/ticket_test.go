package realtime

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zynthara/chok/v3"
)

// TestTicket_RedeemsOnce: a ticket opens one connection, WebSocket or
// stream; the second attempt with it is a 401.
func TestTicket_RedeemsOnce(t *testing.T) {
	s := serveHub(t, newHub(t, testConfig(t), Authorize(openTopics)))
	ticket := s.ticket()
	stream, code, body := s.openSSE("ticket=" + ticket + "&topics=public.a")
	if stream == nil {
		t.Fatalf("first use: %d %s", code, body)
	}
	if _, code, body := s.openSSE("ticket=" + ticket + "&topics=public.a"); code != http.StatusUnauthorized ||
		decodeEnvelope(t, body).Message != "the ticket is invalid, expired or already used" {
		t.Fatalf("second use: %d %s", code, body)
	}
	if code, _ := s.do(http.MethodGet, "/realtime/ws?ticket="+ticket, "Connection", "Upgrade", "Upgrade", "websocket",
		"Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="); code != http.StatusUnauthorized {
		t.Fatalf("a spent ticket on the ws endpoint: %d", code)
	}
}

// TestTicket_Expires: a ticket redeems until TicketTTL has passed since it
// was issued, and the store forgets expired tickets.
func TestTicket_Expires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t)
		cfg.TicketTTL = time.Minute
		h := newHub(t, cfg, Authorize(openTopics))
		s, stop := servePipe(t, h)
		defer stop()
		early, late := s.ticket(), s.ticket()
		time.Sleep(59 * time.Second)
		stream, code, body := s.openSSE("ticket=" + early + "&topics=public.a")
		if stream == nil {
			t.Fatalf("at 59s: %d %s", code, body)
		}
		_ = stream.resp.Body.Close()
		time.Sleep(2 * time.Second)
		if _, code, _ := s.openSSE("ticket=" + late + "&topics=public.a"); code != http.StatusUnauthorized {
			t.Fatalf("at 61s: %d, want 401", code)
		}
		mem := h.tickets.(*memTickets)
		mem.mu.Lock()
		left := len(mem.byID) + len(mem.order)
		mem.mu.Unlock()
		if left != 0 {
			t.Fatalf("%d entries left in the ticket store", left)
		}
		synctest.Wait()
	})
}

// TestTicket_Authn: with Authn the ticket endpoint answers 401 without
// credentials and 200 with them; the ticket carries the principal, which
// may then subscribe to its own user: topic and to no other.
func TestTicket_Authn(t *testing.T) {
	cfg := testConfig(t)
	cfg.TicketTTL = 30 * time.Second
	h := newHub(t, cfg, Authn(fakeAuthn))
	s := serveHub(t, h)
	if code, body := s.do(http.MethodPost, "/realtime/ticket"); code != http.StatusUnauthorized || decodeEnvelope(t, body).Message != "no X-User" {
		t.Fatalf("without credentials: %d %s", code, body)
	}
	before := time.Now()
	code, body := s.do(http.MethodPost, "/realtime/ticket", "X-User", "usr_ada")
	var tr ticketResponse
	if err := json.Unmarshal(body, &tr); err != nil || code != http.StatusOK || !wellFormed(tr.Ticket) ||
		tr.ExpiresAt.Location() != time.UTC || tr.ExpiresAt.Before(before.Add(29*time.Second)) || tr.ExpiresAt.After(time.Now().Add(31*time.Second)) {
		t.Fatalf("ticket %d %s %v", code, body, err)
	}
	stream, code, body := s.openSSE("ticket=" + tr.Ticket + "&topics=user:usr_ada,user:usr_bob")
	if stream == nil {
		t.Fatalf("%d %s", code, body)
	}
	if _, ev := stream.nextEvent(t); ev.Type != "refused" || ev.Topic != "user:usr_bob" {
		t.Fatalf("another user's topic: %+v", ev)
	}
	if err := h.Send(t.Context(), "usr_ada", "note", "hi"); err != nil {
		t.Fatal(err)
	}
	if _, ev := stream.nextEvent(t); ev.Type != "note" || ev.Topic != "user:usr_ada" || string(ev.Data) != `"hi"` {
		t.Fatalf("Send: %+v", ev)
	}
}

// TestTicket_Anonymous: without Authn the ticket endpoint is public and
// its tickets carry no principal: Authorize sees none, and no user: topic
// is anyone's.
func TestTicket_Anonymous(t *testing.T) {
	var sawPrincipal bool
	h := newHub(t, testConfig(t), Authorize(func(ctx context.Context, topic string) error {
		_, sawPrincipal = chok.PrincipalFrom(ctx)
		return openTopics(ctx, topic)
	}))
	s := serveHub(t, h)
	stream, code, body := s.openSSE("ticket=" + s.ticket() + "&topics=public.guests,user:")
	if stream == nil {
		t.Fatalf("%d %s", code, body)
	}
	if _, ev := stream.nextEvent(t); ev.Type != "refused" || ev.Topic != "user:" {
		t.Fatalf("an anonymous user: topic: %+v", ev)
	}
	publish(t, h, "public.guests", "welcome", nil)
	if _, ev := stream.nextEvent(t); ev.Type != "welcome" || sawPrincipal {
		t.Fatalf("%+v, principal seen: %v", ev, sawPrincipal)
	}
	if st := h.Stats(); st.Users != 0 || st.Connections != 1 {
		t.Fatalf("an anonymous connection is no user: %+v", st)
	}
}

// TestTicket_StoreUnavailable: when the ticket store fails, issuing and
// redeeming answer 503.
func TestTicket_StoreUnavailable(t *testing.T) {
	h := newHub(t, testConfig(t), Authorize(openTopics))
	h.tickets = failingTickets{}
	s := serveHub(t, h)
	if code, body := s.do(http.MethodPost, "/realtime/ticket"); code != http.StatusServiceUnavailable || decodeEnvelope(t, body).Reason != "Unavailable" {
		t.Fatalf("issue: %d %s", code, body)
	}
	if _, code, body := s.openSSE("ticket=" + newTicket() + "&topics=public.a"); code != http.StatusServiceUnavailable {
		t.Fatalf("redeem: %d %s", code, body)
	}
}

type failingTickets struct{}

func (failingTickets) put(context.Context, string, claims, time.Duration) error {
	return context.DeadlineExceeded
}

func (failingTickets) take(context.Context, string) (claims, bool, error) {
	return claims{}, false, context.DeadlineExceeded
}
