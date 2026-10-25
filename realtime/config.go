package realtime

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// Config is the realtime section of an application's configuration:
//
//	realtime:
//	  allowed_origins: ["app.example.com"]
//	  ping_interval: 25s
//	  replay: 256
//
// New gives a zero SendBuffer, PingInterval, TicketTTL or
// MaxSubscriptions the defaults below, whether the Config comes from a
// file or from code. Path, Replay and Channel have no zero-value default:
// a Config built in code sets Path, sets Replay to resume at all, and sets
// Channel when it uses Redis.
type Config struct {
	// Path prefixes the three endpoints: {Path}/ticket, {Path}/ws and
	// {Path}/sse. It starts with a slash and does not end with one.
	Path string `yaml:"path" default:"/realtime"`
	// AllowedOrigins admits browser WebSockets from these origins besides
	// the server's own host: host patterns in path.Match syntax, such as
	// app.example.com or *.example.com, matched against the Origin's host
	// (against scheme://host when a pattern has a scheme); "*" admits any
	// origin. Empty admits the same host only. Server-sent event streams
	// are ordinary requests that CORS governs.
	AllowedOrigins []string `yaml:"allowed_origins"`
	// SendBuffer is how many published events may wait for one
	// connection. A connection that falls further behind is a slow
	// consumer: the hub drops it (a WebSocket with close code 4408) and
	// counts it in Stats.Dropped. What a subscription adds — the events it
	// replays, its error or reset events — does not count, but nothing
	// waits beyond SendBuffer + Replay + 2 × MaxSubscriptions events.
	SendBuffer int `yaml:"send_buffer" default:"64"`
	// PingInterval paces the heartbeats: a WebSocket ping, which the
	// client must answer within two intervals or be disconnected, and an
	// SSE comment line.
	PingInterval time.Duration `yaml:"ping_interval" default:"25s"`
	// TicketTTL is how long a ticket from {Path}/ticket can be redeemed.
	TicketTTL time.Duration `yaml:"ticket_ttl" default:"60s"`
	// Replay is how many recent events the hub keeps, across all topics,
	// for clients that resume after an event they saw; 0 turns resuming
	// off, so every resume gets a reset event.
	Replay int `yaml:"replay" default:"256"`
	// MaxSubscriptions caps the topics of one connection: beyond it a
	// WebSocket is closed with 1008 and a stream request answers 400.
	MaxSubscriptions int `yaml:"max_subscriptions" default:"64"`
	// Channel is the Redis channel of the backplane and the prefix of the
	// tickets' keys; deployments sharing one Redis each need their own.
	Channel string `yaml:"channel" default:"chok:realtime"`
}

// The defaults New gives the zero fields of a Config built in code.
const (
	defaultSendBuffer       = 64
	defaultPingInterval     = 25 * time.Second
	defaultTicketTTL        = 60 * time.Second
	defaultMaxSubscriptions = 64
)

// withDefaults fills what a Config built in code leaves at zero; one from
// chok.Load already carries every default.
func (c Config) withDefaults() Config {
	if c.SendBuffer == 0 {
		c.SendBuffer = defaultSendBuffer
	}
	if c.PingInterval == 0 {
		c.PingInterval = defaultPingInterval
	}
	if c.TicketTTL == 0 {
		c.TicketTTL = defaultTicketTTL
	}
	if c.MaxSubscriptions == 0 {
		c.MaxSubscriptions = defaultMaxSubscriptions
	}
	return c
}

// Validate checks the section. It runs when chok.Load reads the
// configuration and again in New, after New's defaults.
func (c *Config) Validate() error {
	var errs []string
	if !strings.HasPrefix(c.Path, "/") || strings.HasSuffix(c.Path, "/") || strings.ContainsAny(c.Path, "{} \t") {
		errs = append(errs, fmt.Sprintf("path must start with / and not end with one, without braces or spaces, got %q", c.Path))
	}
	for i, o := range c.AllowedOrigins {
		if o == "" {
			errs = append(errs, fmt.Sprintf("allowed_origins[%d] is empty", i))
		} else if _, err := path.Match(strings.ToLower(o), ""); errors.Is(err, path.ErrBadPattern) {
			errs = append(errs, fmt.Sprintf("allowed_origins[%d] %q is not a valid pattern", i, o))
		}
	}
	for name, n := range map[string]int{"send_buffer": c.SendBuffer, "max_subscriptions": c.MaxSubscriptions} {
		if n < 1 {
			errs = append(errs, fmt.Sprintf("%s must be at least 1, got %d", name, n))
		}
	}
	for name, d := range map[string]time.Duration{"ping_interval": c.PingInterval, "ticket_ttl": c.TicketTTL} {
		if d <= 0 {
			errs = append(errs, fmt.Sprintf("%s must be positive, got %s", name, d))
		}
	}
	if c.Replay < 0 {
		errs = append(errs, fmt.Sprintf("replay must not be negative (0 turns resuming off), got %d", c.Replay))
	}
	if len(errs) == 0 {
		return nil
	}
	slices.Sort(errs)
	return errors.New(strings.Join(errs, "; "))
}
