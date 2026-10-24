// Command hello is the smallest chok application: health probes, strict
// configuration from chok.yaml and HELLO_* variables, graceful stop.
//
//	HELLO_HTTP_ADDR=127.0.0.1:9090 go run ./examples/hello
package main

import "github.com/zynthara/chok/v3"

func main() {
	chok.New(chok.MustLoad[chok.Base]("hello")).Execute()
}
