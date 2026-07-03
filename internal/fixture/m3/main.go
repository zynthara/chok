// Command m3 is the M3 milestone fixture app (SPEC §10 coexistence
// strategy): the M2 web assembly plus the v2 data layer — db.Module in
// versioned migration mode, a store-backed API with every safety rail
// on, and WithBus events consumed off the kernel bus.
//
//	go run ./internal/fixture/m3     # from the repo root; Ctrl-C to stop
//
// The database is sqlite at m3fixture.db next to the config
// (gitignored); schema comes from the embedded migrations/ set applied
// through the schema_migrations ledger at startup. The CLI face over
// the same project:
//
//	go run ./cmd/chok migrate status \
//	    --config internal/fixture/m3/chok.yaml \
//	    --dir internal/fixture/m3/migrations
//
// prints the ledger plus the framework-table AutoMigrate whitelist.
//
// Endpoints: everything m2 had, plus /api/v1/notes (POST create,
// GET by rid, GET list) backed by store.New[Note], and
// /api/v1/notes/events reporting how many EntityChanged[Note] events
// the bus subscriber has seen (WithBus end to end).
package main

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/debug"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/health"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/metrics"
	"github.com/zynthara/chok/v2/store"
	"github.com/zynthara/chok/v2/store/where"
	"github.com/zynthara/chok/v2/swagger"
	"github.com/zynthara/chok/v2/tracing"
	"github.com/zynthara/chok/v2/web"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Note is the fixture's application table — created by
// migrations/0001_notes.sql, never by AutoMigrate (versioned mode).
type Note struct {
	db.Model
	Title string `json:"title" gorm:"size:200;not null;default:''"`
}

func (Note) RIDPrefix() string { return "note" }

type createNoteReq struct {
	Title string `json:"title" binding:"required,max=200"`
}

type noteResponse struct {
	RID     string `json:"rid"`
	Title   string `json:"title"`
	Version int    `json:"version"`
}

type getNoteReq struct {
	RID string `uri:"rid" binding:"required"`
}

func noteView(n *Note) *noteResponse {
	return &noteResponse{RID: n.RID, Title: n.Title, Version: n.Version}
}

// buildApp is shared by main and the fixture acceptance test.
func buildApp() *chok.App {
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err)
	}

	// createdEvents counts EntityChanged[Note] deliveries — the
	// fixture-visible proof that WithBus publication reaches bus
	// subscribers (and only after commit).
	var createdEvents atomic.Int64

	return chok.New("m3fixture",
		chok.Use(
			log.Module(),
			web.Module(),
			health.Module(),
			metrics.Module(),
			debug.Module(),
			swagger.Module(),
			tracing.Module(),
			db.Module(db.WithMigrations(migrations)),
		),
		chok.Routes(func(r kernel.Router, k kernel.Kernel) error {
			logger, ok := k.Logger().(log.Logger)
			if !ok {
				logger = log.Empty()
			}

			notes := store.New[Note](db.From(k), logger,
				store.WithQueryFields("id", "title", "created_at"),
				store.WithUpdateFields("title"),
				store.WithBus(k.Bus()),
			)

			event.Subscribe(k.Bus(), func(_ context.Context, ev store.EntityChanged[Note]) {
				if ev.Op == store.OpCreate {
					createdEvents.Add(1)
				}
			})

			createNote := func(ctx context.Context, req *createNoteReq) (*noteResponse, error) {
				n := &Note{Title: req.Title}
				if err := notes.Create(ctx, n); err != nil {
					return nil, err
				}
				return noteView(n), nil
			}
			getNote := func(ctx context.Context, req *getNoteReq) (*noteResponse, error) {
				n, err := notes.Get(ctx, store.RID(req.RID))
				if err != nil {
					return nil, err
				}
				return noteView(n), nil
			}
			listNotes := func(ctx context.Context, _ *struct{}) ([]noteResponse, error) {
				page, err := notes.List(ctx, where.WithOrder("id", false))
				if err != nil {
					return nil, err
				}
				out := make([]noteResponse, 0, len(page.Items))
				for i := range page.Items {
					out = append(out, *noteView(&page.Items[i]))
				}
				return out, nil
			}

			api := r.Group("/api/v1")
			api.Handle(http.MethodPost, "/notes", handler.HandleRequest(createNote,
				handler.WithSuccessCode(http.StatusCreated),
				handler.WithSummary("Create note"),
				handler.WithTags("notes"),
			))
			api.Handle(http.MethodGet, "/notes/{rid}", handler.HandleRequest(getNote,
				handler.WithPublic(),
			))
			api.Handle(http.MethodGet, "/notes", handler.HandleRequest(listNotes,
				handler.WithPublic(),
			))
			api.Handle(http.MethodGet, "/notes/events", http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"created":` + strconv.FormatInt(createdEvents.Load(), 10) + "}\n"))
				}))

			k.Logger().Info("fixture: notes routes mounted")
			return nil
		}),
	)
}

func main() {
	// Resolve the fixture's yaml when launched from the repo root (the
	// documented `go run ./internal/fixture/m3` path); an explicit
	// M3FIXTURE_CONFIG always wins (the acceptance test sets it).
	if os.Getenv("M3FIXTURE_CONFIG") == "" {
		_ = os.Setenv("M3FIXTURE_CONFIG", "internal/fixture/m3/chok.yaml")
	}
	_ = os.Setenv("M3FIXTURE_DEBUG_ENABLED", "true")

	buildApp().Execute()
}
