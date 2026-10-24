// Command hello is the smallest chok application with a route: strict
// configuration from chok.yaml and HELLO_* variables, health probes,
// an OpenAPI document at /openapi.json, graceful stop.
//
//	go run ./examples/hello
//	curl 'localhost:8080/hello?name=chok'
package main

import (
	"context"

	"github.com/zynthara/chok/v3"
)

type greeting struct {
	Name string `query:"name" validate:"required"`
}

func main() {
	app := chok.New(chok.MustLoad[chok.Base]("hello"))
	chok.GET(app.Router(), "/hello", func(_ context.Context, req *greeting) (map[string]string, error) {
		return map[string]string{"hello": req.Name}, nil
	}, chok.Public())
	app.Execute()
}
