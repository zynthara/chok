package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMetrics_RecordsRequestsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()

	r := gin.New()
	r.Use(Metrics(reg))
	r.GET("/test", func(c *gin.Context) {
		c.String(200, "ok")
	})

	// Send a request.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify http_requests_total counter was incremented.
	metrics, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, mf := range metrics {
		if mf.GetName() == "http_requests_total" {
			found = true
			if len(mf.GetMetric()) == 0 {
				t.Fatal("http_requests_total has no metrics")
			}
			m := mf.GetMetric()[0]
			if m.GetCounter().GetValue() != 1 {
				t.Fatalf("expected counter=1, got %f", m.GetCounter().GetValue())
			}
			// Check labels.
			labels := make(map[string]string)
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
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
	}
	if !found {
		t.Fatal("http_requests_total metric not found")
	}
}

func TestMetrics_RecordsDuration(t *testing.T) {
	reg := prometheus.NewRegistry()

	r := gin.New()
	r.Use(Metrics(reg))
	r.GET("/dur", func(c *gin.Context) {
		c.String(200, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/dur", nil)
	r.ServeHTTP(w, req)

	metrics, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, mf := range metrics {
		if mf.GetName() == "http_request_duration_seconds" {
			found = true
			if len(mf.GetMetric()) == 0 {
				t.Fatal("http_request_duration_seconds has no metrics")
			}
			m := mf.GetMetric()[0]
			if m.GetHistogram().GetSampleCount() != 1 {
				t.Fatalf("expected 1 observation, got %d", m.GetHistogram().GetSampleCount())
			}
		}
	}
	if !found {
		t.Fatal("http_request_duration_seconds metric not found")
	}
}

func TestMetrics_InFlightGaugeReturnsToZero(t *testing.T) {
	reg := prometheus.NewRegistry()

	r := gin.New()
	r.Use(Metrics(reg))
	r.GET("/flight", func(c *gin.Context) {
		c.String(200, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/flight", nil)
	r.ServeHTTP(w, req)

	metrics, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, mf := range metrics {
		if mf.GetName() == "http_requests_in_flight" {
			if len(mf.GetMetric()) == 0 {
				t.Fatal("http_requests_in_flight has no metrics")
			}
			val := mf.GetMetric()[0].GetGauge().GetValue()
			if val != 0 {
				t.Fatalf("in-flight gauge should be 0 after request completes, got %f", val)
			}
			return
		}
	}
	t.Fatal("http_requests_in_flight metric not found")
}

func TestMetrics_UnmatchedPath(t *testing.T) {
	reg := prometheus.NewRegistry()

	r := gin.New()
	r.Use(Metrics(reg))
	r.GET("/known", func(c *gin.Context) {
		c.String(200, "ok")
	})

	// Hit an unregistered path — should use "unmatched".
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/unknown", nil)
	r.ServeHTTP(w, req)

	metrics, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, mf := range metrics {
		if mf.GetName() == "http_requests_total" {
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "path" && lp.GetValue() == "unmatched" {
						return // found it
					}
				}
			}
		}
	}
	t.Fatal("expected 'unmatched' path label for unknown route")
}
