// Command m2 is the M2 milestone fixture app (SPEC §10 coexistence
// strategy): the smallest assembly that exercises the v2 web stack
// over real HTTP. It replaces the examples/blog smoke test during the
// M1-M4 transition:
//
//	go run ./internal/fixture/m2     # then Ctrl-C for a clean stop
//
// Endpoints: /healthz /livez /readyz (health), /metrics (metrics),
// /componentz (debug, enabled via env below), /swagger/ (spec + UI),
// /hello and a typed /api/v1/posts pair (user routes). The tracing
// module is assembled but disabled by default — /componentz shows the
// disabled state (SPEC §3.1 definition 3).
package main

import (
	"context"
	"net/http"
	"os"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/debug"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/health"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/metrics"
	"github.com/zynthara/chok/v2/swagger"
	"github.com/zynthara/chok/v2/tracing"
	"github.com/zynthara/chok/v2/web"
)

// createPostReq / postResponse feed the swagger baseline: one body-
// bound handler, one path-bound public handler.
type createPostReq struct {
	Title   string `json:"title"   binding:"required,max=200"`
	Content string `json:"content" binding:"required"`
}

type getPostReq struct {
	RID string `uri:"rid" binding:"required"`
}

type postResponse struct {
	RID   string `json:"rid"`
	Title string `json:"title"`
}

func createPost(_ context.Context, req *createPostReq) (*postResponse, error) {
	return &postResponse{RID: "post_1", Title: req.Title}, nil
}

func getPost(_ context.Context, req *getPostReq) (*postResponse, error) {
	return &postResponse{RID: req.RID, Title: "fixture"}, nil
}

// buildApp is shared by main and the fixture acceptance test.
func buildApp() *chok.App {
	return chok.New("m2fixture",
		chok.Use(
			log.Module(),
			web.Module(),
			health.Module(),
			metrics.Module(),
			debug.Module(),
			swagger.Module(),
			tracing.Module(),
		),
		chok.Routes(func(r kernel.Router, k kernel.Kernel) error {
			r.Handle(http.MethodGet, "/hello", http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte("hello from m2\n"))
				}))

			api := r.Group("/api/v1")
			api.Handle(http.MethodPost, "/posts", handler.HandleRequest(createPost,
				handler.WithSuccessCode(http.StatusCreated),
				handler.WithSummary("Create post"),
				handler.WithTags("posts"),
			))
			api.Handle(http.MethodGet, "/posts/{rid}", handler.HandleRequest(getPost,
				handler.WithPublic(),
			))
			k.Logger().Info("fixture: user routes mounted")
			return nil
		}),
	)
}

func main() {
	// Self-contained config: debug defaults to disabled; flip it through
	// the env path so the fixture also exercises env binding without a
	// yaml file next to the binary.
	_ = os.Setenv("M2FIXTURE_DEBUG_ENABLED", "true")

	buildApp().Execute()
}
