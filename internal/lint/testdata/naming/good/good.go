// Package good holds compliant shapes; none of them may be reported.
package good

import "context"

type config struct{ n int }

// The blessed functional option type names.
type (
	Option      func(*config)
	RouteOption func(*config)
	LoadOption  func(*config)
)

// ServerConfig is a yaml struct with the blessed suffix.
type ServerConfig struct {
	Addr string `yaml:"addr"`
}

// Server is what the constructors build.
type Server struct{}

// Hook is a callback over an exported struct, not an option.
type Hook func(*Server)

// Handler is a func type that is not an option.
type Handler func(ctx context.Context, n int) error

// Size is an option constructor; those need no prefix.
func Size(n int) Option { return func(c *config) { c.n = n } }

// New is a blessed constructor.
func New(opts ...Option) *Server { return &Server{} }

// MustNew is a blessed constructor.
func MustNew(opts ...Option) *Server { return New(opts...) }

// Open is a blessed constructor.
func Open(cfg ServerConfig) (*Server, error) { return &Server{}, nil }

// MustOpen is a blessed constructor.
func MustOpen(cfg ServerConfig) *Server { return &Server{} }

// NewServerFromEnv only needs the New prefix.
func NewServerFromEnv() (*Server, error) { return &Server{}, nil }

// Lookup returns a value, so it is not a constructor.
func Lookup(ctx context.Context) (Server, bool) { return Server{}, false }

// Within merely starts with the letters of the banned prefix.
func Within(n int) bool { return n > 0 }

// WithExempted breaks the With rule; the fixture test exempts it.
func WithExempted() {}
