// Command m4 is the M4 milestone fixture app (SPEC §10 coexistence
// strategy): the full-battery assembly — every migrated module in one
// process, wired the way a real deployment would be.
//
//	go run ./internal/fixture/m4     # from the repo root; Ctrl-C to stop
//
// The assembly demonstrates the M4 deliverables end to end:
//
//   - account mounts the /auth surface (register/login over real HTTP)
//     and account.Authn(k) guards the business route below;
//   - authz boots casbin with bootstrap seeding and audit_enabled=true
//     (7.E): every policy mutation — the seed grants included — lands
//     in audit_logs, whose schema the audit module's Migrator created;
//   - audit's admin API (GET /audit/logs) sits behind
//     RequireAuthz("audit","read"): anonymous 401, ungranted 403,
//     granted 200 — fail-closed at every step;
//   - the purge job rides the scheduler module; cache runs memory-only;
//     redis is assembled but disabled in yaml (the registration-disabled
//     model: visible in /componentz as `disabled`, no socket needed).
package main

import (
	"context"
	"net/http"
	"os"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/audit"
	"github.com/zynthara/chok/v2/auth"
	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/cache"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/debug"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/health"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/metrics"
	"github.com/zynthara/chok/v2/redis"
	"github.com/zynthara/chok/v2/scheduler"
	"github.com/zynthara/chok/v2/swagger"
	"github.com/zynthara/chok/v2/tracing"
	"github.com/zynthara/chok/v2/web"
)

type whoamiResponse struct {
	Subject string `json:"subject"`
	Name    string `json:"name"`
}

// buildApp is shared by main and the fixture acceptance test.
func buildApp() *chok.App {
	return chok.New("m4fixture",
		chok.Use(
			log.Module(),
			web.Module(),
			health.Module(),
			metrics.Module(),
			debug.Module(),
			swagger.Module(),
			tracing.Module(),
			db.Module(),
			redis.Module(),
			cache.Module(),
			scheduler.Module(),
			audit.Module(),
			authz.Module(),
			account.Module(),
		),
		chok.Routes(func(r kernel.Router, k kernel.Kernel) error {
			// A protected business route behind the blessed guard:
			// token verification + ActiveCheck (revocation enforced).
			whoami := func(ctx context.Context, _ *struct{}) (*whoamiResponse, error) {
				p, _ := auth.PrincipalFrom(ctx)
				return &whoamiResponse{Subject: p.Subject, Name: p.Name}, nil
			}
			api := r.Group("/api/v1", account.Authn(k))
			api.Handle(http.MethodGet, "/whoami", handler.HandleRequest(whoami,
				handler.WithSummary("Who am I"),
				handler.WithTags("fixture"),
			))

			k.Logger().Info("fixture: business routes mounted")
			return nil
		}),
	)
}

func main() {
	// Resolve the fixture's yaml when launched from the repo root (the
	// documented `go run ./internal/fixture/m4` path); an explicit
	// M4FIXTURE_CONFIG always wins (the acceptance test sets it).
	if os.Getenv("M4FIXTURE_CONFIG") == "" {
		_ = os.Setenv("M4FIXTURE_CONFIG", "internal/fixture/m4/chok.yaml")
	}
	_ = os.Setenv("M4FIXTURE_DEBUG_ENABLED", "true")

	buildApp().Execute()
}
