package parts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/log"
)

// healthKernel is a mock Kernel returning a configurable HealthReport.
// It exists alongside mockKernel so a test can control the aggregate
// directly without constructing a full Registry.
type healthKernel struct {
	*mockKernel
	report   component.HealthReport
	readyErr error // configurable ReadyCheck result
}

func (h *healthKernel) Health(_ context.Context) component.HealthReport { return h.report }
func (h *healthKernel) ReadyCheck(_ context.Context) error              { return h.readyErr }

func TestHealthComponent_DefaultPath(t *testing.T) {
	h := NewHealthComponent("")
	if h.Path() != "/healthz" {
		t.Fatalf("default path = %q want %q", h.Path(), "/healthz")
	}
}

func TestHealthComponent_CustomPath(t *testing.T) {
	h := NewHealthComponent("/health")
	if h.Path() != "/health" {
		t.Fatalf("custom path = %q want %q", h.Path(), "/health")
	}
}

func TestHealthComponent_Mount_ServesAggregate(t *testing.T) {
	k := &healthKernel{
		mockKernel: newMockKernel(nil),
		report: component.HealthReport{
			Status: component.HealthOK,
			Components: map[string]component.HealthStatus{
				"db":    {Status: component.HealthOK},
				"redis": {Status: component.HealthOK},
			},
		},
	}

	h := NewHealthComponent("")
	if err := h.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	if err := h.Mount(r); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body component.HealthReport
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body.Status != component.HealthOK || len(body.Components) != 2 {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestHealthComponent_Mount_Returns503OnDown(t *testing.T) {
	k := &healthKernel{
		mockKernel: newMockKernel(nil),
		report: component.HealthReport{
			Status: component.HealthDown,
			Components: map[string]component.HealthStatus{
				"db": {Status: component.HealthDown, Error: "connection refused"},
			},
		},
	}

	h := NewHealthComponent("")
	if err := h.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	if err := h.Mount(r); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when any component Down, got %d", w.Code)
	}
}

func TestHealthComponent_Mount_Returns200OnDegraded(t *testing.T) {
	// Degraded stays 200 — orchestrators don't pull the pod out, but
	// operators see the flag in the body.
	k := &healthKernel{
		mockKernel: newMockKernel(nil),
		report: component.HealthReport{
			Status: component.HealthDegraded,
		},
	}

	h := NewHealthComponent("")
	if err := h.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	if err := h.Mount(r); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for degraded, got %d", w.Code)
	}
}

func TestHealthComponent_Livez_AlwaysOK(t *testing.T) {
	k := &healthKernel{
		mockKernel: newMockKernel(nil),
		report: component.HealthReport{
			Status: component.HealthDown,
		},
	}

	h := NewHealthComponent("")
	if err := h.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	if err := h.Mount(r); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/livez", nil))

	// livez is always 200 regardless of component health.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHealthComponent_Readyz_OKWhenHealthy(t *testing.T) {
	k := &healthKernel{
		mockKernel: newMockKernel(nil),
		report: component.HealthReport{
			Status: component.HealthOK,
			Components: map[string]component.HealthStatus{
				"db": {Status: component.HealthOK},
			},
		},
	}

	h := NewHealthComponent("")
	if err := h.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	if err := h.Mount(r); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHealthComponent_Readyz_503WhenDown(t *testing.T) {
	k := &healthKernel{
		mockKernel: newMockKernel(nil),
		report: component.HealthReport{
			Status: component.HealthDown,
		},
	}

	h := NewHealthComponent("")
	if err := h.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	if err := h.Mount(r); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestHealthComponent_Readyz_503DuringShutdown(t *testing.T) {
	k := &healthKernel{
		mockKernel: newMockKernel(nil),
		report: component.HealthReport{
			Status: component.HealthOK, // even though everything is OK...
		},
	}

	h := NewHealthComponent("")
	if err := h.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	if err := h.Mount(r); err != nil {
		t.Fatal(err)
	}

	// Before shutdown: readyz returns 200.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("before shutdown: expected 200, got %d", w.Code)
	}

	// Trigger shutdown.
	h.SetShuttingDown()

	// After shutdown: readyz returns 503.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("during shutdown: expected 503, got %d", w.Code)
	}
}

func TestHealthComponent_Mount_RejectsBadRouter(t *testing.T) {
	h := NewHealthComponent("")
	k := newMockKernel(nil)
	if err := h.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	if err := h.Mount("not a gin router"); err == nil {
		t.Fatal("Mount should reject non-router argument")
	}
}

// Ensure log import stays used by tests that share the mockKernel helper.
var _ = log.Empty
