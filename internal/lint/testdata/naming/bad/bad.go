// Package bad breaks every naming rule once.
package bad

type config struct{ addr string }

// FooOption is a functional option under a name that is not blessed.
type FooOption func(*config)

// Setter has the functional option shape under an unrelated name.
type Setter func(*config)

// ServerOpts carries yaml tags but is not named *Config.
type ServerOpts struct {
	Addr string `yaml:"addr"`
}

// WithAddr uses the banned With prefix.
func WithAddr(addr string) FooOption { return func(c *config) { c.addr = addr } }

// Client is built by the two misnamed constructors below.
type Client struct{}

// MakeClient is a constructor without a blessed prefix.
func MakeClient() *Client { return &Client{} }

// BuildClient is a constructor without a blessed prefix.
func BuildClient() (*Client, error) { return &Client{}, nil }
