// Package promutil contains the small Prometheus registration helpers shared
// by framework modules. It is internal so collector ownership remains with the
// module that defines each metric.
package promutil

import "github.com/prometheus/client_golang/prometheus"

// RegisterOrReuseCounterVec registers cv or returns the already-registered
// CounterVec with the same descriptor. On any other registration failure it
// returns cv together with the error; callers may choose whether observability
// failure is fatal for their surface.
func RegisterOrReuseCounterVec(reg prometheus.Registerer, cv *prometheus.CounterVec) (*prometheus.CounterVec, error) {
	if err := reg.Register(cv); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing, nil
			}
		}
		return cv, err
	}
	return cv, nil
}

// RegisterOrReuseHistogramVec is RegisterOrReuseCounterVec for HistogramVec.
func RegisterOrReuseHistogramVec(reg prometheus.Registerer, hv *prometheus.HistogramVec) (*prometheus.HistogramVec, error) {
	if err := reg.Register(hv); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(*prometheus.HistogramVec); ok {
				return existing, nil
			}
		}
		return hv, err
	}
	return hv, nil
}

// RegisterOrReuseGauge registers g or returns the already-registered Gauge.
func RegisterOrReuseGauge(reg prometheus.Registerer, g prometheus.Gauge) (prometheus.Gauge, error) {
	if err := reg.Register(g); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(prometheus.Gauge); ok {
				return existing, nil
			}
		}
		return g, err
	}
	return g, nil
}

// RegisterOrReuseGaugeVec is RegisterOrReuseCounterVec for GaugeVec.
func RegisterOrReuseGaugeVec(reg prometheus.Registerer, gv *prometheus.GaugeVec) (*prometheus.GaugeVec, error) {
	if err := reg.Register(gv); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(*prometheus.GaugeVec); ok {
				return existing, nil
			}
		}
		return gv, err
	}
	return gv, nil
}
