package chok_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/zynthara/chok/v3"
)

// The smallest application: defaults, chok.yaml and HELLO_* variables,
// health probes, graceful stop.
func Example() {
	app := chok.New(chok.MustLoad[chok.Base]("hello"))
	_ = app // app.Execute() blocks until SIGINT or SIGTERM
}

// Config embeds Base and adds the application's own section.
func ExampleLoad() {
	type Config struct {
		chok.Base
		Greeting struct {
			Text string `yaml:"text" default:"hello"`
		} `yaml:"greeting"`
	}
	cfg, err := chok.Load[Config]("example", chok.Args(nil))
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(cfg.Greeting.Text, cfg.HTTP.Addr)
	// Output: hello :8080
}

type worker struct{}

func (worker) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (worker) Health(context.Context) error    { return nil }

// Components are values; Use registers their lifecycle.
func ExampleApp_Use() {
	base := chok.MustLoad[chok.Base]("example", chok.Args(nil))
	app := chok.New(base)
	app.Use(worker{})

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	fmt.Println(rec.Code)
	// Output: 200
}

type outOfStock struct{ sku string }

func (e outOfStock) Error() string { return "out of stock: " + e.sku }

func (e outOfStock) APIError() *chok.Error {
	return chok.ErrConflict.Msgf("%s is out of stock", e.sku).With("sku", e.sku)
}

// An error that implements Coder chooses its own response.
func ExampleCoder() {
	var err error = outOfStock{sku: "A-1"}
	var c chok.Coder
	if errors.As(err, &c) {
		e := c.APIError()
		fmt.Println(e.Code, e.Reason, e.Message, e.Metadata["sku"])
	}
	// Output: 409 Conflict A-1 is out of stock A-1
}
