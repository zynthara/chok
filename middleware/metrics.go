package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics returns a gin middleware that instruments HTTP requests with
// the RED (Rate, Errors, Duration) metrics — the industry-standard
// golden signals for any HTTP service:
//
//   - http_requests_total{method, path, status}       — counter
//   - http_request_duration_seconds{method, path}     — histogram
//   - http_requests_in_flight                         — gauge
//
// reg is typically obtained from MetricsComponent.PrometheusRegistry().
// The middleware is safe to use with any prometheus.Registerer, and is
// idempotent across re-initialisation: on AlreadyRegisteredError (which
// happens when HTTPComponent.Init runs after a prior Init/Close cycle)
// the existing collectors are reused instead of panicking.
func Metrics(reg prometheus.Registerer) gin.HandlerFunc {
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

	requestsTotal = registerOrReuseCounterVec(reg, requestsTotal)
	requestDuration = registerOrReuseHistogramVec(reg, requestDuration)
	requestsInFlight = registerOrReuseGauge(reg, requestsInFlight)

	return func(c *gin.Context) {
		start := time.Now()
		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}

		requestsInFlight.Inc()
		defer requestsInFlight.Dec()

		c.Next()

		status := strconv.Itoa(c.Writer.Status())
		method := c.Request.Method
		elapsed := time.Since(start).Seconds()

		requestsTotal.WithLabelValues(method, path, status).Inc()
		requestDuration.WithLabelValues(method, path).Observe(elapsed)
	}
}

// registerOrReuseCounterVec registers cv with reg. On
// AlreadyRegisteredError it returns the existing collector so writes
// reach the same instance that's being scraped. Returns the input on
// success, the existing one on collision, or cv (unregistered) if an
// unexpected error occurs — in the last case the middleware still
// functions but the metric is not exported.
func registerOrReuseCounterVec(reg prometheus.Registerer, cv *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(cv); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
	}
	return cv
}

func registerOrReuseHistogramVec(reg prometheus.Registerer, hv *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := reg.Register(hv); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(*prometheus.HistogramVec); ok {
				return existing
			}
		}
	}
	return hv
}

func registerOrReuseGauge(reg prometheus.Registerer, g prometheus.Gauge) prometheus.Gauge {
	if err := reg.Register(g); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(prometheus.Gauge); ok {
				return existing
			}
		}
	}
	return g
}
