package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/internal/promutil"
)

// Metrics returns a middleware that instruments HTTP requests with
// the RED (Rate, Errors, Duration) metrics — the industry-standard
// golden signals for any HTTP service:
//
//   - http_requests_total{method, path, status}       — counter
//   - http_request_duration_seconds{method, path}     — histogram
//   - http_requests_in_flight                         — gauge
//
// reg is typically the metrics module's Prometheus registry — web.Module
// wires that automatically when the module is assembled. The middleware
// is safe to use with any prometheus.Registerer, and is idempotent
// across re-initialisation: on AlreadyRegisteredError the existing
// collectors are reused instead of panicking.
//
// The path label is the matched route pattern ({rid} style since M2 —
// declared change, dashboards keying on :rid labels need updating);
// unmatched requests record "unmatched".
func Metrics(reg prometheus.Registerer) func(http.Handler) http.Handler {
	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests.",
	}, []string{"method", "path", "status"})

	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	requestsInFlight := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "http_requests_in_flight",
		Help: "Number of HTTP requests currently being processed.",
	})

	requestsTotal, _ = promutil.RegisterOrReuseCounterVec(reg, requestsTotal)
	requestDuration, _ = promutil.RegisterOrReuseHistogramVec(reg, requestDuration)
	requestsInFlight, _ = promutil.RegisterOrReuseGauge(reg, requestsInFlight)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			requestsInFlight.Inc()
			defer requestsInFlight.Dec()

			next.ServeHTTP(w, r)

			path := ctxval.RoutePatternFrom(r.Context())
			if path == "" {
				path = "unmatched"
			}
			status := strconv.Itoa(statusOf(w))
			method := r.Method
			elapsed := time.Since(start).Seconds()

			requestsTotal.WithLabelValues(method, path, status).Inc()
			requestDuration.WithLabelValues(method, path).Observe(elapsed)
		})
	}
}
