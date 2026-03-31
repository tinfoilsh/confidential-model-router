package manager

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsRejectedTotal tracks the total number of requests rejected due to overload
	RequestsRejectedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_requests_rejected_total",
			Help: "Total number of requests rejected due to backend overload",
		},
		[]string{"model"},
	)

	// RetryAfterSeconds tracks the distribution of Retry-After values returned to clients
	RetryAfterSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "router_retry_after_seconds",
			Help:    "Distribution of Retry-After values returned to clients (in seconds)",
			Buckets: []float64{10, 30, 60, 120, 300, 600}, // 10s, 30s, 1m, 2m, 5m, 10m
		},
		[]string{"model"},
	)

	// BackendQueueDepth tracks the current queue depth at each backend enclave
	BackendQueueDepth = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "router_backend_queue_depth",
			Help: "Current number of requests waiting in the backend queue",
		},
		[]string{"model", "enclave"},
	)

	// BackendOverloaded tracks whether a backend is currently in overload state (1=overloaded, 0=healthy)
	BackendOverloaded = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "router_backend_overloaded",
			Help: "Whether the backend is currently overloaded (1=overloaded, 0=healthy)",
		},
		[]string{"model", "enclave"},
	)

	// OverloadEventsTotal tracks the total number of times a backend entered overload state
	OverloadEventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_overload_events_total",
			Help: "Total number of times a backend entered overload state",
		},
		[]string{"model", "enclave"},
	)

	// RecoveryEventsTotal tracks the total number of times a backend recovered from overload
	RecoveryEventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_recovery_events_total",
			Help: "Total number of times a backend recovered from overload state",
		},
		[]string{"model", "enclave"},
	)

	// RateLimitDemotionsTotal tracks requests deprioritized due to token rate limits
	RateLimitDemotionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_ratelimit_demotions_total",
			Help: "Total number of requests sent with lower vLLM priority due to token rate limits",
		},
		[]string{"model"},
	)

	// CircuitBreakerState tracks the current state of each enclave's circuit breaker (0=closed, 1=open, 2=half-open)
	CircuitBreakerState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "router_circuit_breaker_state",
			Help: "Current circuit breaker state (0=closed/healthy, 1=open/unhealthy, 2=half-open/probing)",
		},
		[]string{"model", "enclave"},
	)

	// ProxySuccessTotal tracks successful proxy responses per enclave
	ProxySuccessTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_proxy_success_total",
			Help: "Total number of successful proxy responses (status < 500)",
		},
		[]string{"model", "enclave"},
	)

	// ProxyFailureTotal tracks failed proxy responses and transport errors per enclave.
	// The "reason" label classifies the failure: timeout, tls_mismatch, connection_refused,
	// connection_reset, dns_error, tls_error, canceled, transport_error, or http_<status>.
	ProxyFailureTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_proxy_failure_total",
			Help: "Total number of failed proxy responses (status >= 500 or transport error)",
		},
		[]string{"model", "enclave", "reason"},
	)
)
