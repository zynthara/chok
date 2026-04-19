package parts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zynthara/chok/component"
)

func TestMetricsComponent_DefaultPath(t *testing.T) {
	m := NewMetricsComponent("")
	if m.Path() != "/metrics" {
		t.Fatalf("default path = %q want %q", m.Path(), "/metrics")
	}
}

func TestMetricsComponent_Mount_ServesPrometheusFormat(t *testing.T) {
	m := NewMetricsComponent("").WithoutDefaults()

	// Register a trivial counter so the scrape has something specific
	// we can assert on — it isolates the test from any default
	// collector behaviour.
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "chok_test_counter",
		Help: "phase 5 test counter",
	})
	m.PrometheusRegistry().MustRegister(counter)
	counter.Inc()
	counter.Inc()

	if err := m.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	if err := m.Mount(r); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "chok_test_counter 2") {
		t.Fatalf("counter not present in scrape output; body:\n%s", body)
	}
}

func TestMetricsComponent_DefaultRegistry_IncludesGoCollector(t *testing.T) {
	m := NewMetricsComponent("")
	if err := m.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	if err := m.Mount(r); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))

	// A Go-runtime collector emits go_* metrics; assert at least one
	// canonical metric is present.
	body := w.Body.String()
	if !strings.Contains(body, "go_goroutines") {
		t.Fatalf("expected Go runtime metrics, body lacked go_goroutines:\n%s", body)
	}
}

func TestMetricsComponent_PrometheusRegistry_IsMutable(t *testing.T) {
	m := NewMetricsComponent("").WithoutDefaults()
	c := prometheus.NewGauge(prometheus.GaugeOpts{Name: "gauge_a", Help: "x"})
	m.PrometheusRegistry().MustRegister(c)
	// Should not panic on second, distinct collector.
	c2 := prometheus.NewGauge(prometheus.GaugeOpts{Name: "gauge_b", Help: "y"})
	m.PrometheusRegistry().MustRegister(c2)
}

func TestMetricsComponent_Mount_RejectsBadRouter(t *testing.T) {
	m := NewMetricsComponent("")
	if err := m.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if err := m.Mount("not a gin router"); err == nil {
		t.Fatal("Mount should reject non-router argument")
	}
}

func TestMetricsComponent_Close_NoOp(t *testing.T) {
	m := NewMetricsComponent("")
	_ = m.Init(context.Background(), newMockKernel(nil))
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close should be nil, got %v", err)
	}
}

// Ensure component package import is live across Phase 5 tests.
var _ = component.HealthOK
