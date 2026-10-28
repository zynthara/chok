package realtime

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/zynthara/chok/v3"
)

// Mount registers the three endpoints under Config.Path:
//
//	POST {Path}/ticket   a one-time ticket; behind Authn when given, else public
//	GET  {Path}/ws       a WebSocket, opened with ?ticket=…[&topics=a,b]
//	GET  {Path}/sse      a server-sent event stream: ?ticket=…&topics=a,b
//
// The ws and sse routes run no authentication middleware, so the OpenAPI
// document lists them without security: the ticket in their query is
// what admits a client, so a browser's WebSocket and EventSource never
// carry the long-lived token, nor does any URL. The ticket route lists
// the schemes of the Authn given, if any.
func (h *Hub) Mount(r chok.Router) {
	g := r.Group(h.cfg.Path)
	tag := chok.Tags("realtime")
	ticket := []chok.RouteOption{tag, chok.Summary("Issue a one-time ticket for the ws and sse endpoints")}
	if h.authn != nil {
		ticket = append(ticket, chok.Use(h.authn))
	}
	chok.POST(g, "/ticket", h.issue, ticket...)
	// The two streams are typed routes so that the OpenAPI document lists
	// them with their parameters; the route middleware serves them, as a
	// stream cannot be a handler's return value.
	chok.GET(g, "/ws", notServed[wsQuery, chok.None], chok.Use(serve(h.serveWS)), tag,
		chok.Status(http.StatusSwitchingProtocols),
		chok.Summary("Open a WebSocket of events (needs a ticket): frames subscribe and unsubscribe, each event comes as a JSON frame"))
	chok.GET(g, "/sse", notServed[sseQuery, Event], chok.Use(serve(h.serveSSE)), tag,
		chok.Summary("Stream events as text/event-stream (needs a ticket): each data line is an Event"))
}

// wsQuery documents the query of GET {Path}/ws.
type wsQuery struct {
	Ticket string `query:"ticket" validate:"required"`
	Topics string `query:"topics"`
}

// sseQuery documents the query and headers of GET {Path}/sse.
type sseQuery struct {
	Ticket      string `query:"ticket" validate:"required"`
	Topics      string `query:"topics" validate:"required"`
	LastEventID string `query:"last_event_id"`
	Resume      string `header:"Last-Event-ID"`
}

// notServed stands in for the stream routes' handlers, which their route
// middleware replaces.
func notServed[T, R any](context.Context, *T) (R, error) {
	var zero R
	return zero, chok.ErrInternal.Msgf("realtime: the stream handler did not run")
}

// serve turns h into route middleware that answers the request itself.
func serve(h http.HandlerFunc) chok.Middleware {
	return func(http.Handler) http.Handler { return h }
}

// ticketResponse is the body of POST {Path}/ticket.
type ticketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

// issue serves POST {Path}/ticket: a ticket bound to the request's
// principal, if any, redeemable once within Config.TicketTTL.
func (h *Hub) issue(ctx context.Context, _ *chok.None) (ticketResponse, error) {
	var c claims
	if p, ok := chok.PrincipalFrom(ctx); ok {
		c.Principal = &p
	}
	t := newTicket()
	expires := time.Now().Add(h.cfg.TicketTTL)
	if err := h.tickets.put(ctx, t, c, h.cfg.TicketTTL); err != nil {
		return ticketResponse{}, errUnavailable.Wrap(err)
	}
	return ticketResponse{Ticket: t, ExpiresAt: expires.UTC()}, nil
}

// The errors the endpoints answer with.
var (
	errUnavailable = chok.NewError(http.StatusServiceUnavailable, "Unavailable", "realtime is unavailable, try again later")
	errClosing     = chok.NewError(http.StatusServiceUnavailable, "Unavailable", "the server is shutting down, reconnect shortly")
	errNoTicket    = chok.ErrUnauthorized.Msgf("a ticket is required: get one from the ticket endpoint")
	errBadTicket   = chok.ErrUnauthorized.Msgf("the ticket is invalid, expired or already used")
	errNoUpgrade   = chok.ErrBadRequest.Msgf("a WebSocket upgrade request is required")
)

// redeem takes the request's ticket: its claims, or the error to answer.
func (h *Hub) redeem(r *http.Request) (claims, error) {
	t := r.URL.Query().Get("ticket")
	switch {
	case t == "":
		return claims{}, errNoTicket
	case !wellFormed(t):
		return claims{}, errBadTicket
	}
	c, ok, err := h.tickets.take(r.Context(), t)
	if err != nil {
		return claims{}, errUnavailable.Wrap(err)
	}
	if !ok {
		return claims{}, errBadTicket
	}
	return c, nil
}

// withClaims returns ctx carrying the ticket's principal, which
// Authorize sees.
func withClaims(ctx context.Context, c claims) context.Context {
	if c.Principal != nil {
		return chok.WithPrincipal(ctx, *c.Principal)
	}
	return ctx
}

// splitTopics reads topics=a,b (the parameter may repeat).
func splitTopics(values []string) []string {
	var out []string
	for _, v := range values {
		for t := range strings.SplitSeq(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}
