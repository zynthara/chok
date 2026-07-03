package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/health"
	"github.com/zynthara/chok/v2/kernel"
)

type sickComp struct {
	optional bool
}

func (s *sickComp) Describe() kernel.Descriptor {
	return kernel.Descriptor{Kind: "sick", Optional: s.optional}
}
func (s *sickComp) Init(context.Context, kernel.Kernel) error { return nil }
func (s *sickComp) Close(context.Context) error               { return nil }
func (s *sickComp) Health(context.Context) error              { return errors.New("dead") }

func get(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	return w, w.Body.String()
}

func TestHealth_MountsCanonicalEndpoints(t *testing.T) {
	tk := choktest.NewTestKernel(t, "", health.Module())
	for _, want := range []string{"GET /healthz", "GET /livez", "GET /readyz"} {
		found := false
		for _, p := range tk.Router.Patterns() {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %q in %v", want, tk.Router.Patterns())
		}
	}
}

func TestHealth_PathFromConfig(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
health:
  path: /internal/health
`, health.Module())
	if _, ok := tk.Router.Handler(http.MethodGet, "/internal/health"); !ok {
		t.Fatalf("configured path must mount: %v", tk.Router.Patterns())
	}
}

func TestHealth_HealthzReportsAggregate(t *testing.T) {
	tk := choktest.NewTestKernel(t, "", health.Module())
	h, _ := tk.Router.Handler(http.MethodGet, "/healthz")
	w, body := get(t, h)
	if w.Code != http.StatusOK {
		t.Fatalf("healthy app must 200, got %d: %s", w.Code, body)
	}
	var resp struct {
		Status     string `json:"status"`
		Components []struct {
			Component string `json:"component"`
			Status    string `json:"status"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "up" || len(resp.Components) == 0 {
		t.Fatalf("unexpected report: %s", body)
	}
}

func TestHealth_RequiredDown_503(t *testing.T) {
	tk := choktest.NewTestKernel(t, "", health.Module(), &sickComp{})
	h, _ := tk.Router.Handler(http.MethodGet, "/healthz")
	w, body := get(t, h)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("required-down must 503, got %d: %s", w.Code, body)
	}
	if !strings.Contains(body, "dead") {
		t.Fatalf("report must carry the probe error: %s", body)
	}
}

func TestHealth_ReadyzFlipsOnDrain(t *testing.T) {
	mod := health.Module()
	tk := choktest.NewTestKernel(t, "", mod)

	ready, _ := tk.Router.Handler(http.MethodGet, "/readyz")
	if w, body := get(t, ready); w.Code != http.StatusOK {
		t.Fatalf("serving app must be ready: %d %s", w.Code, body)
	}

	// The kernel broadcasts Drain at the start of the draining phase;
	// invoke the same contract directly.
	mod.(kernel.Drainer).Drain(context.Background())
	if w, body := get(t, ready); w.Code != http.StatusServiceUnavailable || !strings.Contains(body, "draining") {
		t.Fatalf("draining must 503: %d %s", w.Code, body)
	}

	// livez keeps answering 200 during drain (liveness ≠ readiness).
	live, _ := tk.Router.Handler(http.MethodGet, "/livez")
	if w, _ := get(t, live); w.Code != http.StatusOK {
		t.Fatalf("livez must stay 200 during drain, got %d", w.Code)
	}
}
