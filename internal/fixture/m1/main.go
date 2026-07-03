// Command m1 is the M1 milestone fixture app (SPEC §10 coexistence
// strategy): the smallest assembly that exercises the v2 kernel with
// the four migrated modules. It replaces the examples/blog smoke test
// during the M1-M4 transition:
//
//	go run ./internal/fixture/m1     # then Ctrl-C for a clean stop
//
// No real HTTP serving happens in M1 — mounting goes to a recording
// router double (kernel.Router is an interface); endpoint
// reachability is an M2 acceptance item.
package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/debug"
	"github.com/zynthara/chok/v2/health"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/metrics"
)

func main() {
	// Self-contained config: the debug module defaults to disabled;
	// flip it through the env path so the fixture also exercises env
	// binding without needing a yaml file next to the binary.
	_ = os.Setenv("M1FIXTURE_DEBUG_ENABLED", "true")

	router := choktest.NewTestRouter()

	app := chok.New("m1fixture",
		chok.Use(
			log.Module(),
			health.Module(),
			metrics.Module(),
			debug.Module(),
			choktest.NewRouterProviderComponent(router),
		),
		chok.Routes(func(r kernel.Router, k kernel.Kernel) error {
			r.Handle(http.MethodGet, "/hello", http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte("hello from m1\n"))
				}))
			k.Logger().Info("fixture: user routes mounted")
			return nil
		}),
	)

	// Surface what mounted where after shutdown — the M1-visible proof
	// that the mount phase ordered mounters and user routes correctly.
	defer func() {
		fmt.Println("mounted routes:")
		for _, p := range router.Patterns() {
			fmt.Println("  ", p)
		}
	}()

	app.Execute()
}
