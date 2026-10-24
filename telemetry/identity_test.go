package telemetry

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/internal/tracectx"
)

// TestAudit_RecordsTheRequestTrace: in a binary that links telemetry an
// audit event logged while serving a request carries the request's trace
// id; identity reads it through internal/tracectx, without OpenTelemetry.
func TestAudit_RecordsTheRequestTrace(t *testing.T) {
	if tracectx.TraceID == nil {
		t.Fatal("telemetry's init sets the trace hook")
	}
	if id := tracectx.TraceID(t.Context()); id != "" {
		t.Fatalf("a context outside any trace: %q", id)
	}
	tel, _ := tracing(t, metricsOn())
	cfg := identity.Config{SigningKey: "telemetry-test-signing-key-of-32-bytes!!"}
	cfg.Audit.Enabled = true
	idn, err := identity.New(cfg, dbtest.Open(t))
	if err != nil {
		t.Fatal(err)
	}
	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		app.Use(tel, idn)
		chok.POST(app.Router(), "/publish", func(ctx context.Context, _ *chok.None) (chok.None, error) {
			return chok.None{}, idn.Audit().LogSync(ctx, identity.Event{Action: "post.publish"})
		})
	})
	req, _ := http.NewRequest(http.MethodPost, "/publish", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if resp := c.Do(req); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /publish = %d", resp.StatusCode)
	}
	page, err := idn.Audit().ListFromQuery(t.Context(), url.Values{"action": {"post.publish"}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	if got := page.Items[0].TraceID; got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("the event's trace id %q", got)
	}
}
