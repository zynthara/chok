# Realtime: events in the browser as they happen

The `realtime` battery pushes events to browsers without a reload: an
order that changed, a chat message, a background job that finished. One
`realtime.Hub` serves two transports from the same topics — a
**WebSocket**, for pages that also send, and **server-sent events**
(SSE), for pages that only listen, through the browser's `EventSource` —
and with Redis it spans every replica of a deployment.

This page is the five-minute path from the browser's side. The complete
example app is [`examples/chatter`](../examples/chatter): chat rooms over a
WebSocket, a jobs panel over SSE, a bot answering from a worker. Every
key of the `realtime` section is in [config.md](config.md#realtimeconfig--section-realtime),
every symbol in [api.md](api.md#realtime).

## The server

A hub is a value you construct and register, like every battery. This
app lets signed-in users follow the topic `news`, publishes to it from a
handler, and can ping one user's own connections:

```go
package main

import (
	"context"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/realtime"
)

type Config struct {
	chok.Base
	DB       db.Config       `yaml:"db"`
	Auth     identity.Config `yaml:"auth"`
	Realtime realtime.Config `yaml:"realtime"`
}

type story struct {
	Title string `json:"title" validate:"required,max=200"`
}

func main() {
	cfg := chok.MustLoad[Config]("live")
	app := chok.New(cfg.Base)
	d := db.MustOpen(cfg.DB)
	auth := identity.MustNew(cfg.Auth, d)
	hub := realtime.MustNew(cfg.Realtime,
		realtime.Authn(auth.Authn()), // a ticket carries the signed-in user
		realtime.Authorize(func(ctx context.Context, topic string) error {
			if topic == "news" {
				return nil // anyone signed in may follow the news
			}
			return chok.ErrForbidden.Msgf("no topic %s here", topic)
		}))
	app.Use(d, auth, hub) // POST /realtime/ticket, GET /realtime/ws, GET /realtime/sse

	api := app.Group("/api", auth.Authn())
	chok.POST(api, "/stories", func(ctx context.Context, s *story) (chok.None, error) {
		return chok.None{}, hub.Publish(ctx, "news", "story.posted", s)
	})
	chok.POST(api, "/ping-me", func(ctx context.Context, _ *chok.None) (chok.None, error) {
		p, _ := chok.PrincipalFrom(ctx)
		return chok.None{}, hub.Send(ctx, p.UserID, "pong", nil) // topic user:<UserID>
	})
	app.Execute()
}
```

```yaml
# chok.yaml; the signing key comes from LIVE_AUTH_SIGNING_KEY
db:
  driver: sqlite
  sqlite:
    path: live.db
realtime:
  replay: 256   # the last 256 events stay in memory for clients that reconnect
```

`Publish` returns at once: each subscriber has a send queue, and a
connection that lets `send_buffer` events (64) pile up is dropped as a
slow consumer rather than slowing anyone down. Publish after a commit —
in `d.AfterCommit` — so nobody sees a change that rolled back.

## Tickets

A browser cannot put an `Authorization` header on a WebSocket or an
`EventSource`, and a long-lived token in a URL ends up in logs. So the
page trades its token for a **ticket** first:

```js
async function ticket() {
  const res = await fetch("/realtime/ticket", {method: "POST", headers: {Authorization: "Bearer " + token}});
  return (await res.json()).ticket; // {"ticket": "…", "expires_at": "…"}
}
```

A ticket opens **one** connection, within `ticket_ttl` (60s), and binds
it to the user it was issued to. Get a new one for every connection and
every reconnect. Without `realtime.Authn` the ticket endpoint is public
and its tickets anonymous — for pages that serve guests.

## In the browser: EventSource

For a page that only listens, `EventSource` is the simplest client. Each
event's name is the published type, its data the whole event as JSON:

```js
let last = ""; // the ID of the last event seen: where to resume
async function follow() {
  const es = new EventSource(`/realtime/sse?topics=news&ticket=${await ticket()}&last_event_id=${last}`);
  es.addEventListener("story.posted", (e) => {
    last = e.lastEventId;
    const ev = JSON.parse(e.data); // {id, topic, type, data, at}
    show(ev.data.title);
  });
  es.addEventListener("reset", () => reloadStories()); // too far behind: fetch the state anew
  // A refused topic is an "error" event with data; EventSource fires its
  // own errors under the same name, without data, when the connection
  // breaks. It would then reconnect by itself, but with the same URL,
  // whose ticket is spent: close it and open a fresh one instead.
  es.addEventListener("error", (e) => {
    if (e.data) return console.warn(JSON.parse(e.data).data.message);
    es.close();
    setTimeout(follow, 3000);
  });
}
follow();
```

- `topics=a,b` lists the topics; a refused one arrives as an `error`
  event and the others stream on. If every topic is refused the request
  answers `403`; a bad or spent ticket `401`; a bad topic list `400`.
- `last_event_id` (or the `Last-Event-ID` header, which `EventSource`
  sends on its own reconnects) resumes after that event: the hub replays
  what it kept since then, or sends one `reset` per topic when that event
  is no longer kept — then the page reads the state from the API again.
- The stream starts with `retry: 3000` and writes a comment line every
  `ping_interval` (25s), so proxies do not take it for idle. It carries
  `X-Accel-Buffering: no` for nginx, and outlives `http.write_timeout`;
  with `http.request_timeout` set, it ends at that limit and the page
  reconnects.

## In the browser: WebSocket

A WebSocket also takes the client's frames, so a page changes its topics
without reconnecting:

```js
let ws, room = "room:7", last = "";
async function connect() {
  ws = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/realtime/ws?ticket=${await ticket()}`);
  ws.onopen = () => ws.send(JSON.stringify(last ? {subscribe: [room], after: last} : {subscribe: [room]}));
  ws.onmessage = (m) => {
    const ev = JSON.parse(m.data); // one event per frame: {id, topic, type, data, at}
    if (ev.type === "reset") return reloadRoom();
    if (ev.type === "error") return console.warn(ev.topic, ev.data.message);
    last = ev.id;
    show(ev);
  };
  ws.onclose = (e) => setTimeout(connect, e.code === 1008 ? 30000 : 2000);
}
connect();
// later: ws.send(JSON.stringify({unsubscribe: ["room:7"]}));
```

- The client sends `{"subscribe":["a","b"],"after":"<event id>"}` and
  `{"unsubscribe":["a"]}`; `?topics=a,b` on the URL subscribes at once.
- The hub pings every `ping_interval`; browsers answer by themselves. A
  client that does not answer within two intervals is cut off.
- Close codes: **1001** the server is stopping; **1008** a frame the
  protocol does not know, a bad topic, or more than `max_subscriptions`
  topics; **4408** a slow consumer. Reconnect with a new ticket and
  `after`, and nothing is lost that the hub still keeps.
- A browser's `Origin` must be the server's own host or match
  `allowed_origins` (`*.example.com`; `*` admits any), else the upgrade
  answers `403`.

## Try it with curl

With the server above running and a token in `$TOKEN` (register or log in
under `/auth`):

```sh
T=$(curl -s -X POST localhost:8080/realtime/ticket -H "Authorization: Bearer $TOKEN" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["ticket"])')
curl -sN "localhost:8080/realtime/sse?topics=news&ticket=$T"
# retry: 3000
```

and in another terminal:

```sh
curl -s -X POST localhost:8080/api/stories -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"title":"chok ships realtime"}'
```

The first terminal prints the event:

```text
id: 7c1d05b2e9a4-1
event: story.posted
data: {"id":"7c1d05b2e9a4-1","topic":"news","type":"story.posted","data":{"title":"chok ships realtime"},"at":"…"}
```

## Topics and who may follow them

A topic is 1 to 128 of `A-Z a-z 0-9 _ . : -` — `news`,
`order:ord_123`, `room:7`. `realtime.Authorize` decides every
subscription, with the ticket's user in its context
(`chok.PrincipalFrom`); returning a `*chok.Error` or a `chok.Coder` names
the reason and message the client sees. Without `Authorize` nothing is
open except `user:<UserID>`, the connection's own user's topic, which is
reserved: only that user's connections follow it, whatever `Authorize`
says, and `hub.Send(ctx, userID, type, data)` publishes to it.

## Background jobs, live

`jobs.Observe` reports every run of the periodic jobs, the workers and
the cleanup as it starts, finishes, fails or is skipped. The batteries do
not import each other; the application joins them in a closure:

```go fragment
jobs.Observe(func(ev jobs.Event) {
	_ = hub.Publish(context.Background(), "jobs", ev.Kind, ev) // run.started, run.finished, ...
})
```

A page following `jobs` shows the background work live — chatter's jobs
panel is exactly that. A worker sweeps its topic every `poll_interval`;
filter out the sweeps that found nothing (`ev.Messages == 0`) unless you
want a heartbeat.

## Several replicas

Without Redis, tickets and the replay buffer live in one process: run one
replica, or pin each browser to one. With several, give every hub the
same Redis client:

```go fragment
rc := cache.MustNew(cfg.Cache) // cache.redis.enabled: true; its client is shared
hub := realtime.MustNew(cfg.Realtime, realtime.Authn(auth.Authn()), realtime.Redis(rc.Redis()))
app.Use(d, auth, rc, hub)
```

- Tickets live in Redis under `{channel}:ticket:<ticket>` (`SET NX` with
  the TTL, `GETDEL` to redeem): any replica redeems a ticket, and only
  once.
- Every `Publish` reaches the local subscribers and is `PUBLISH`ed on
  `realtime.channel`; the other replicas deliver it to theirs and keep it
  for resuming, and skip their own echo. A client that reconnects to
  another replica resumes there — unless that replica started after the
  event, which the client sees as a `reset`.
- `Start` holds the subscription and reconnects after a loss, logging it
  at Warn; `/healthz` reports the hub down while it is lost, and the App
  starts serving only once it is up. Give every deployment sharing one
  Redis its own `channel`.

## Reference

| Endpoint | What it does |
|---|---|
| `POST /realtime/ticket` | `200 {"ticket","expires_at"}`: a ticket for one connection, behind `realtime.Authn` when given |
| `GET /realtime/ws?ticket=…[&topics=a,b]` | A WebSocket: subscribe / unsubscribe frames in, one event per frame out |
| `GET /realtime/sse?ticket=…&topics=a,b[&last_event_id=…]` | A `text/event-stream`: `id`, `event` (the type) and `data` (the event) per event |

The prefix is `realtime.path`. Every connection is one line in the access
log when it ends, with its duration: status `101` for a WebSocket, `200`
for a stream. `hub.Stats()` counts the connections, users, topics,
publishes, deliveries and dropped slow consumers.
