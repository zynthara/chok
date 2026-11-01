package realtime

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
)

func TestSessions_ExpireWebSocketAndSSE(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := testConfig(t)
		cfg.ConnectionTTL = time.Second
		cfg.PingInterval = time.Hour
		h := newHub(t, cfg, Authorize(openTopics))
		s, stop := servePipe(t, h)
		defer stop()
		ws := s.dialWS("topics=public.a")
		wsDone := make(chan error, 1)
		go func() { _, _, err := ws.Read(t.Context()); wsDone <- err }()
		stream, code, body := s.openSSE("ticket=" + s.ticket() + "&topics=public.a")
		if stream == nil {
			t.Fatalf("SSE: %d %s", code, body)
		}
		sseDone := make(chan struct{})
		go func() {
			defer close(sseDone)
			for {
				if _, err := stream.next(); err != nil {
					return
				}
			}
		}()
		synctest.Wait()
		if h.Stats().Connections != 2 {
			t.Fatal("sessions not registered")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if h.Stats().Connections != 0 {
			t.Fatal("expired sessions remain registered")
		}
		if err := <-wsDone; websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("expiry close: %v", err)
		}
		<-sseDone
	})
}

func TestDisconnect_EndsOnlyTheSelectedUsersSessions(t *testing.T) {
	h := newHub(t, testConfig(t), Authn(fakeAuthn))
	s := serveHub(t, h)
	a := s.dialWS("topics=user:usr_a", "X-User", "usr_a")
	b := s.dialWS("topics=user:usr_b", "X-User", "usr_b")
	eventually(t, "subscriptions", func() bool { return h.Stats().Topics == 2 })
	h.Disconnect("usr_a")
	if _, _, err := a.Read(t.Context()); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("revocation: %v", err)
	}
	if err := h.Send(context.Background(), "usr_b", "private.updated", 1); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, b); ev.Type != "private.updated" {
		t.Fatalf("other user affected: %+v", ev)
	}
}
