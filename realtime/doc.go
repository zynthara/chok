// Package realtime is chok's battery for pushing events to browsers as
// they happen: a Hub of topics that clients subscribe to over a WebSocket
// or a server-sent event stream, opened with one-time tickets, resumed
// after a reconnect from a short replay buffer, and shared by the
// replicas of a deployment through Redis.
//
//	hub := realtime.MustNew(cfg.Realtime,
//		realtime.Authn(auth.Authn()), // a ticket carries the signed-in user
//		realtime.Authorize(func(ctx context.Context, topic string) error {
//			p, _ := chok.PrincipalFrom(ctx)
//			if canWatch(ctx, p.UserID, topic) {
//				return nil
//			}
//			return chok.ErrForbidden.Msgf("not your order")
//		}),
//	)
//	app.Use(d, auth, hub) // POST /realtime/ticket, GET /realtime/ws and /realtime/sse
//
//	// After the write commits, from any code that has the hub:
//	err := hub.Publish(ctx, "order:"+o.RID, "order.updated", o)
//
// # Tickets
//
// A browser cannot put an Authorization header on a WebSocket or an
// EventSource, and a long-lived token in a URL ends up in proxy logs. So
// the page first POSTs to {Path}/ticket with its token (Authn guards the
// endpoint) and gets a ticket: 32 random bytes, redeemable once, within
// Config.TicketTTL, by the ws or sse endpoint, which binds the
// connection to the ticket's principal. Without Authn the endpoint is
// public and tickets are anonymous.
//
// # Topics
//
// A topic is 1 to 128 of A-Z a-z 0-9 _ . : -, such as order:ord_123 or
// jobs. Authorize decides who may subscribe to what; without it, a
// connection may subscribe to nothing but its own user's topic, user:<UserID>,
// which is reserved: only that user's connections subscribe to it, and
// Send publishes to it. A refused subscription is a refused event to the
// client, not a closed connection. The event types refused, reset and
// error are the hub's: Publish refuses them.
//
// # In the browser
//
// Server-sent events are the simpler transport when the page only
// listens. EventSource reconnects by itself, but a ticket is good once:
// on error, close it and open a new one with a fresh ticket and the last
// event's ID.
//
//	let last = "";
//	async function listen() {
//	  const {ticket} = await (await fetch("/realtime/ticket", {method: "POST", headers: {Authorization: "Bearer " + token}})).json();
//	  const es = new EventSource(`/realtime/sse?ticket=${ticket}&topics=jobs&last_event_id=${last}`);
//	  es.addEventListener("run.finished", e => { last = e.lastEventId; show(JSON.parse(e.data)); });
//	  es.addEventListener("reset", reload); // too far behind: fetch the state anew
//	  es.addEventListener("refused", e => console.warn(JSON.parse(e.data).data.message));
//	  es.onerror = () => { es.close(); setTimeout(listen, 3000); }; // the connection broke
//	}
//
// A WebSocket also takes the client's frames: {"subscribe":["room:7"],
// "after":"<last event id>"} and {"unsubscribe":["room:7"]}; each event
// arrives as one JSON text frame, an Event.
//
// # Delivery
//
// Each event gets an ID, "<origin>-<seq>", and goes to the subscribers'
// send queues; the hub never waits for a slow connection — one that has
// Config.SendBuffer events waiting is dropped (WebSocket close code 4408).
// The last Config.Replay events, across topics, stay in memory: a client
// that resumes after an event ID gets the events of its topics since
// then, or a reset event per topic when that event is gone, which tells
// it to fetch the state anew. Events are not stored anywhere else: a
// client that must not miss one reads the state from the API after a
// reset.
//
// # Several replicas
//
// With Redis, tickets live in Redis — any replica redeems a ticket, once
// — and every Publish also goes to the channel Config.Channel, where the
// other replicas pick it up for their subscribers and their replay
// buffers. Start runs that subscription, reconnecting after a loss;
// Health fails while it is down. Without Redis, run one replica or pin
// clients to one. Resuming on a replica that just started finds an empty
// buffer: the client gets a reset, as designed.
package realtime
