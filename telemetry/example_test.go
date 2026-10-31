package telemetry_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/telemetry"
)

type greeting struct {
	Name string `path:"name"`
}

// Telemetry next to the App: Use puts its middleware in the default
// stack, and the running App serves the metrics at
// http://127.0.0.1:9464/metrics, a port of their own that only this host
// reaches; Instrument adds the database. A Config from chok.Load has the
// metrics on; this one is built in code. The App here is not run: the
// registry is read in process.
func Example() {
	var cfg telemetry.Config
	cfg.Metrics.Enabled = true
	tel := telemetry.MustNew(cfg)
	defer func() { _ = tel.Close(context.Background()) }()

	quiet := slog.New(slog.DiscardHandler)
	d := db.MustOpen(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: ":memory:"}}, db.Logger(quiet))
	defer func() { _ = d.Close(context.Background()) }()
	tel.Instrument(d)

	app := chok.New(chok.Base{}, chok.Logger(quiet))
	app.Use(tel, d)
	chok.GET(app.Router(), "/hello/{name}", func(ctx context.Context, req *greeting) (string, error) {
		var one int
		err := d.Unsafe(ctx).Raw("SELECT 1").Scan(&one).Error
		return "hello " + req.Name, err
	})
	app.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/hello/ann", nil))

	rec := httptest.NewRecorder()
	promhttp.HandlerFor(tel.Registry(), promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(rec.Body.String()) {
		if strings.HasPrefix(line, "http_server_request_duration_seconds_count") ||
			strings.HasPrefix(line, "db_client_operation_duration_seconds_count") {
			fmt.Print(line)
		}
	}
	// Output:
	// db_client_operation_duration_seconds_count{db_collection_name="",db_operation_name="SELECT",db_system_name="sqlite",error_type=""} 1
	// http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/hello/{name}"} 1
}
