package middleware

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zynthara/chok/v2/internal/ctxval"
)

// metricsHandler simulates a routed handler: fills the route-pattern
// slot (as the web router does at dispatch) and writes a response.
func metricsHandler(pattern string, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pattern != "" {
			ctxval.RoutePatternHolder(r.Context()).Set(pattern)
		}
		w.WriteHeader(status)
	})
}

// serveWithPattern sends one request through mw-wrapped h with the
// pattern slot installed (as the web root handler does).
func serveWithPattern(h http.Handler) *trackingRecorder {
	w := newTrackingRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	ctx, _ := ctxval.WithRoutePattern(req.Context())
	h.ServeHTTP(w, req.WithContext(ctx))
	return w
}

func gatherLabels(t *testing.T, reg *prometheus.Registry, name string) (float64, map[string]string) {
	t.Helper()
	metrics, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range metrics {
		if mf.GetName() != name {
			continue
		}
		if len(mf.GetMetric()) == 0 {
			t.Fatalf("%s has no metrics", name)
		}
		m := mf.GetMetric()[0]
		labels := make(map[string]string)
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		switch {
		case m.GetCounter() != nil:
			return m.GetCounter().GetValue(), labels
		case m.GetHistogram() != nil:
			return float64(m.GetHistogram().GetSampleCount()), labels
		case m.GetGauge() != nil:
			return m.GetGauge().GetValue(), labels
		}
	}
	t.Fatalf("%s metric not found", name)
	return 0, nil
}

func TestMetrics_RecordsRequestsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := chain(metricsHandler("/test", 200), Metrics(reg))

	w := serveWithPattern(h)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	val, labels := gatherLabels(t, reg, "http_requests_total")
	if val != 1 {
		t.Fatalf("expected counter=1, got %f", val)
	}
	if labels["method"] != "GET" {
		t.Fatalf("expected method=GET, got %s", labels["method"])
	}
	if labels["path"] != "/test" {
		t.Fatalf("expected path=/test, got %s", labels["path"])
	}
	if labels["status"] != "200" {
		t.Fatalf("expected status=200, got %s", labels["status"])
	}
}

func TestMetrics_RecordsDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := chain(metricsHandler("/test", 200), Metrics(reg))

	serveWithPattern(h)

	count, labels := gatherLabels(t, reg, "http_request_duration_seconds")
	if count != 1 {
		t.Fatalf("expected 1 duration sample, got %f", count)
	}
	if labels["path"] != "/test" {
		t.Fatalf("expected path=/test, got %s", labels["path"])
	}
}

func TestMetrics_InFlightGaugeReturnsToZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := chain(metricsHandler("/test", 200), Metrics(reg))

	serveWithPattern(h)

	val, _ := gatherLabels(t, reg, "http_requests_in_flight")
	if val != 0 {
		t.Fatalf("in-flight gauge should be 0 after request completes, got %f", val)
	}
}

func TestMetrics_UnmatchedPath(t *testing.T) {
	reg := prometheus.NewRegistry()
	// Handler never fills the pattern slot — the unmatched case.
	h := chain(metricsHandler("", 404), Metrics(reg))

	serveWithPattern(h)

	_, labels := gatherLabels(t, reg, "http_requests_total")
	if labels["path"] != "unmatched" {
		t.Fatalf("expected path=unmatched, got %s", labels["path"])
	}
	if labels["status"] != "404" {
		t.Fatalf("expected status=404, got %s", labels["status"])
	}
}

func TestMetrics_ReRegistrationReusesCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	h1 := chain(metricsHandler("/test", 200), Metrics(reg))
	serveWithPattern(h1)
	// Second construction against the same registry must not panic and
	// must keep writing into the same collectors.
	h2 := chain(metricsHandler("/test", 200), Metrics(reg))
	serveWithPattern(h2)

	val, _ := gatherLabels(t, reg, "http_requests_total")
	if val != 2 {
		t.Fatalf("expected shared counter=2 after re-registration, got %f", val)
	}
}
