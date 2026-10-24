package telemetry

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Config is the telemetry section of an application's configuration:
//
//	telemetry:
//	  metrics:
//	    path: /metrics           # served by default
//	  tracing:
//	    enabled: true
//	    exporter: otlp
//	    endpoint: http://otel-collector:4318
//	    service_name: blog
//	    sample_ratio: 0.25
//	  logs:
//	    otel: true
//
// New gives the zero fields of a Config built in code the defaults
// below: Metrics.Path, Tracing.Exporter, Tracing.Endpoint and
// Tracing.SampleRatio. The switches have no zero-value default: a Config
// built in code sets Metrics.Enabled to serve /metrics.
type Config struct {
	Metrics struct {
		// Enabled records the HTTP and database metrics, with the Go
		// runtime and process collectors, and serves them on Path in the
		// Prometheus text format. Off, the registry stays empty.
		Enabled bool `yaml:"enabled" default:"true"`
		// Path is where the App serves the metrics.
		Path string `yaml:"path" default:"/metrics"`
	} `yaml:"metrics"`
	Tracing struct {
		// Enabled records a span for every request and, through
		// Instrument, every SQL statement, and exports them.
		Enabled bool `yaml:"enabled"`
		// Exporter is where spans go: stdout (one JSON object per span,
		// for development) or otlp (OTLP over HTTP to Endpoint).
		Exporter string `yaml:"exporter" default:"stdout" validate:"oneof=stdout otlp"`
		// Endpoint is the OpenTelemetry collector's base URL, as in
		// OTEL_EXPORTER_OTLP_ENDPOINT: spans are posted to
		// <endpoint>/v1/traces and, with Logs.OTel, log records to
		// <endpoint>/v1/logs. https uses TLS with the system roots. The
		// other OTEL_EXPORTER_OTLP_* variables (headers, certificates,
		// timeout) still apply.
		Endpoint string `yaml:"endpoint" default:"http://localhost:4318"`
		// ServiceName is the service.name of the spans and log records;
		// empty takes OTEL_SERVICE_NAME, else the executable's name.
		ServiceName string `yaml:"service_name"`
		// SampleRatio is the fraction of new traces recorded, in (0, 1];
		// a request whose traceparent says sampled is always recorded.
		SampleRatio float64 `yaml:"sample_ratio" default:"1"`
	} `yaml:"tracing"`
	Logs struct {
		// OTel copies every record the App's loggers write (those New
		// builds from the log section, the access log included) to
		// OpenTelemetry Logs, exported over OTLP to Tracing.Endpoint
		// with the trace and span id of the request; the log output
		// itself is unchanged.
		OTel bool `yaml:"otel"`
	} `yaml:"logs"`
}

// The defaults New gives the zero fields of a Config built in code.
const (
	defaultMetricsPath = "/metrics"
	defaultExporter    = "stdout"
	defaultEndpoint    = "http://localhost:4318"
	defaultSampleRatio = 1.0
)

func (c Config) withDefaults() Config {
	if c.Metrics.Path == "" {
		c.Metrics.Path = defaultMetricsPath
	}
	if c.Tracing.Exporter == "" {
		c.Tracing.Exporter = defaultExporter
	}
	if c.Tracing.Endpoint == "" {
		c.Tracing.Endpoint = defaultEndpoint
	}
	if c.Tracing.SampleRatio == 0 {
		c.Tracing.SampleRatio = defaultSampleRatio
	}
	return c
}

// Validate checks the parts of the section that are switched on; the
// exporter's name, a validate tag, is checked either way. It runs when
// chok.Load reads the configuration and again in New.
func (c *Config) Validate() error {
	var errs []error
	if c.Metrics.Enabled && !strings.HasPrefix(c.Metrics.Path, "/") {
		errs = append(errs, fmt.Errorf("metrics.path must start with /, got %q", c.Metrics.Path))
	}
	if c.Tracing.Enabled {
		if r := c.Tracing.SampleRatio; r <= 0 || r > 1 {
			errs = append(errs, fmt.Errorf("tracing.sample_ratio must be in (0, 1], got %v", r))
		}
	}
	if (c.Tracing.Enabled && c.Tracing.Exporter == "otlp") || c.Logs.OTel {
		if err := checkEndpoint(c.Tracing.Endpoint); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkEndpoint accepts the collector's base URL: http or https, a host,
// and no signal path, which the exporters append.
func checkEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("tracing.endpoint must be an http or https URL such as http://localhost:4318, got %q", endpoint)
	}
	if p := strings.TrimRight(u.Path, "/"); strings.HasSuffix(p, "/v1/traces") || strings.HasSuffix(p, "/v1/logs") {
		return fmt.Errorf("tracing.endpoint is the collector's base URL, without /v1/traces or /v1/logs (they are appended), got %q", endpoint)
	}
	return nil
}

// signalURL is the endpoint with the signal's path appended.
func signalURL(endpoint, signal string) string {
	u, _ := url.Parse(endpoint) // checked by Validate
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/" + signal
	return u.String()
}
