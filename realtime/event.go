package realtime

import (
	// The Event's data field is encoding/json's RawMessage, which is
	// jsontext.Value: JSON text the hub encodes with encoding/json/v2.
	"encoding/json"
	"time"
)

// Event is one event as a client receives it: a WebSocket text frame
// carries one, and so does the data line of a server-sent event.
type Event struct {
	// ID is "<origin>-<seq>": origin is a random id the publishing Hub
	// draws in New, seq counts that Hub's events. A client resumes after
	// the ID it saw last; IDs from different replicas are not ordered.
	// The error and reset events the hub sends itself have no ID.
	ID string `json:"id"`
	// Topic is the topic the event was published to.
	Topic string `json:"topic"`
	// Type is the application's name for the event, such as
	// order.updated or run.finished. The hub sends two of its own: error,
	// for a subscription refused, and reset, for a resume that went too
	// far back.
	Type string `json:"type"`
	// Data is the published value as JSON; empty when nil was published.
	Data json.RawMessage `json:"data,omitzero"`
	// At is when the event was published, in UTC.
	At time.Time `json:"at"`
}

// Stats is a snapshot of a Hub: its connections and its counters since
// New.
type Stats struct {
	// Connections counts the open WebSocket and SSE connections.
	Connections int `json:"connections"`
	// Users counts the distinct users among the connections that carry a
	// principal.
	Users int `json:"users"`
	// Topics counts the topics with at least one subscriber.
	Topics int `json:"topics"`
	// Published counts this process's Publish calls, Send's included.
	Published uint64 `json:"published"`
	// Delivered counts the events put on connections' send queues, live
	// or replayed; error and reset events are not counted.
	Delivered uint64 `json:"delivered"`
	// Dropped counts the connections dropped as slow consumers.
	Dropped uint64 `json:"dropped"`
	// Backplane is "" without Redis, else connected or disconnected.
	Backplane string `json:"backplane"`
}

// The event types the hub sends itself; Publish refuses them.
const (
	typeError = "error"
	typeReset = "reset"
)

// nameRule describes what validName admits.
const nameRule = "1 to 128 characters of A-Z, a-z, 0-9, _ . : -"

// validName reports whether s may be a topic or an event type: 1 to 128
// of A-Z a-z 0-9 _ . : - — nothing that could break a frame or an SSE
// line.
func validName(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := range len(s) {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// frame is an event encoded once for every connection that gets it.
type frame struct {
	id, topic, typ string
	json           []byte // the Event as JSON, compact: one line
}
