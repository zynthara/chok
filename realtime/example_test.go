package realtime_test

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/realtime"
)

// A hub whose topic news anyone may follow, a stream reading it the way a
// browser's EventSource does, and an event published to it.
func Example() {
	hub := realtime.MustNew(realtime.Config{Path: "/realtime", Replay: 256},
		realtime.Authorize(func(ctx context.Context, topic string) error {
			if topic == "news" {
				return nil
			}
			return chok.ErrForbidden.Msgf("no topic %s", topic)
		}))
	app := chok.New(chok.Base{}, chok.Logger(slog.New(slog.DiscardHandler)))
	app.Use(hub) // POST /realtime/ticket, GET /realtime/ws and /realtime/sse
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()
	defer func() { _ = hub.Close(context.Background()) }()

	// The page: a ticket, then the stream (no Authn: tickets are anonymous).
	resp, err := http.Post(srv.URL+"/realtime/ticket", "", nil)
	if err != nil {
		panic(err)
	}
	var t struct {
		Ticket string `json:"ticket"`
	}
	if err := json.UnmarshalRead(resp.Body, &t); err != nil {
		panic(err)
	}
	_ = resp.Body.Close()
	stream, err := http.Get(srv.URL + "/realtime/sse?topics=news&ticket=" + t.Ticket)
	if err != nil {
		panic(err)
	}
	defer func() { _ = stream.Body.Close() }()
	lines := bufio.NewScanner(stream.Body)
	lines.Scan() // retry: 3000 — the stream is subscribed

	// The server, after a commit:
	_ = hub.Publish(context.Background(), "news", "headline", map[string]string{"title": "chok ships realtime"})

	for lines.Scan() {
		if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
			var ev realtime.Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				panic(err)
			}
			fmt.Println(ev.Topic, ev.Type, string(ev.Data))
			break
		}
	}
	// Output: news headline {"title":"chok ships realtime"}
}
