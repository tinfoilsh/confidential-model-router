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
)
