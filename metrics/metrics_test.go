package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/metrics"
)

func scrape(t *testing.T, tk *choktest.TestKernel, path string) string {
	t.Helper()
	h, ok := tk.Router.Handler(http.MethodGet, path)
	if !ok {
		t.Fatalf("no %s handler: %v", path, tk.Router.Patterns())
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("scrape %d", w.Code)
	}
	return w.Body.String()
}

func TestMetrics_ExposesRuntimeAndLifecycle(t *testing.T) {
	tk := choktest.NewTestKernel(t, "", metrics.Module())
	body := scrape(t, tk, "/metrics")

	for _, want := range []string{
		"go_goroutines",       // GoCollector wired
		"chok_uptime_seconds", // uptime gauge
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape missing %q", want)
		}
	}

	// The lifecycle gauge is fed by the bus asynchronously (gauge
	// updates carry no veto and may lag startup), so a scrape right
	// after boot can race the subscriber — poll like the
	// reload-counter test does.
	want := `chok_component_up{component="metrics"} 1`
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(scrape(t, tk, "/metrics"), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("scrape missing %q", want)
}

func TestMetrics_PathFromConfig(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
metrics:
  path: /internal/metrics
`, metrics.Module())
	if _, ok := tk.Router.Handler(http.MethodGet, "/internal/metrics"); !ok {
		t.Fatalf("configured path must mount: %v", tk.Router.Patterns())
	}
}

func TestMetrics_ReloadCounterFollowsBus(t *testing.T) {
	tk := choktest.NewTestKernel(t, "", metrics.Module())

	event.Publish(context.Background(), tk.Bus(), kernel.ReloadApplied{})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(scrape(t, tk, "/metrics"), "chok_reload_total 1") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reload counter must follow bus events")
}

func TestMetrics_RegistryExposedForCustomCollectors(t *testing.T) {
	tk := choktest.NewTestKernel(t, "", metrics.Module())
	mc, ok := kernel.Get[*metrics.Component](tk, "metrics")
	if !ok {
		t.Fatal("typed Get must reach the metrics component")
	}
	if mc.Registry() == nil {
		t.Fatal("prometheus registry must be exposed")
	}
}
