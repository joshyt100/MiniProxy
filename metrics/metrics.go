package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_requests_total",
			Help: "Total number of requests proxied.",
		},
		[]string{"upstream", "status_code"},
	)

	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "proxy_request_duration_seconds",
			Help:    "Request latency in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"upstream"},
	)

	ActiveConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "proxy_active_connections",
			Help: "Number of active connections per upstream.",
		},
		[]string{"upstream"},
	)

	UpstreamHealthy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "proxy_upstream_healthy",
			Help: "1 if upstream is healthy, 0 if not.",
		},
		[]string{"upstream"},
	)
)

func Register() {
	prometheus.MustRegister(
		RequestsTotal,
		RequestDuration,
		ActiveConnections,
		UpstreamHealthy,
	)
}
