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

	// TTFTSeconds tracks time to first response body byte for streaming
	// requests, split by caller priority class for SLA tracking
	TTFTSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "router_ttft_seconds",
			Help:    "Time to first response body byte for streaming requests",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 7.5, 10, 15, 30, 60},
		},
		[]string{"model", "priority"},
	)

	// InterTokenSeconds tracks gaps between streamed response chunks,
	// split by caller priority class for SLA tracking
	InterTokenSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "router_inter_token_seconds",
			Help:    "Gap between streamed response body chunks",
			Buckets: []float64{0.005, 0.01, 0.015, 0.02, 0.025, 0.03, 0.04, 0.05, 0.075, 0.1, 0.15, 0.25, 0.5, 1},
		},
		[]string{"model", "priority"},
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

	// RateLimitDemotionsTotal tracks requests deprioritized due to soft per-key request-count limits
	RateLimitDemotionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_ratelimit_demotions_total",
			Help: "Total number of requests sent with lower vLLM priority due to per-key request-count limits",
		},
		[]string{"model"},
	)

	// RateLimitRejectionsTotal tracks requests rejected due to hard per-key request-count limits
	RateLimitRejectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_ratelimit_rejections_total",
			Help: "Total number of requests rejected with 429 due to hard per-key request-count limits",
		},
		[]string{"model"},
	)

	// RateLimitTokenDemotionsTotal tracks requests deprioritized due to soft per-key uncached-prompt-token limits
	RateLimitTokenDemotionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_ratelimit_token_demotions_total",
			Help: "Total number of requests sent with lower vLLM priority due to per-key uncached prompt token limits",
		},
		[]string{"model"},
	)

	// RateLimitTokenRejectionsTotal tracks requests rejected due to hard per-key uncached-prompt-token limits
	RateLimitTokenRejectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_ratelimit_token_rejections_total",
			Help: "Total number of requests rejected with 429 due to hard per-key uncached prompt token limits",
		},
		[]string{"model"},
	)

	// PriorityAssignmentsTotal tracks requests assigned configured vLLM priority
	PriorityAssignmentsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_priority_assignments_total",
			Help: "Total number of requests sent with configured vLLM priority",
		},
		[]string{"model"},
	)

	// PriorityOverloadAdmitsTotal tracks configured-priority requests admitted while all backends reported overload
	PriorityOverloadAdmitsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_priority_overload_admits_total",
			Help: "Total number of configured-priority requests admitted despite backend overload",
		},
		[]string{"model"},
	)

	// ReservedPoolServesTotal tracks serves on models with reservations by
	// landing pool: reserved, spilled (reserved org overflowing to shared),
	// or shared.
	ReservedPoolServesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_reserved_pool_serves_total",
			Help: "Total serves on models with enclave reservations, by landing pool (reserved, spilled, shared)",
		},
		[]string{"model", "pool"},
	)

	// CacheSaltInjectionsTotal tracks requests injected with a derived cache_salt
	CacheSaltInjectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_cache_salt_injections_total",
			Help: "Total number of requests injected with a derived cache_salt, labeled by derivation mode (tenant or user)",
		},
		[]string{"model", "mode"},
	)

	// RouteContextLookupFailuresTotal tracks failed route-context lookups to the control plane
	RouteContextLookupFailuresTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_route_context_lookup_failures_total",
			Help: "Total number of failed route-context lookups to the control plane",
		},
		[]string{"model", "reason"},
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
	// connection_reset, dns_error, tls_error, transport_error, or http_<status>. These are
	// all inputs to the circuit breaker. Client cancellations and slow-header observations
	// are tracked separately in ClientCancellationsTotal and SlowHeadersTotal.
	ProxyFailureTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_proxy_failure_total",
			Help: "Total number of failed proxy responses (status >= 500 or transport error)",
		},
		[]string{"model", "enclave", "reason"},
	)

	// ClientCancellationsTotal tracks requests that ended because the client
	// disconnected or aborted. Not a backend fault; does not trip the breaker.
	ClientCancellationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_client_cancellations_total",
			Help: "Total number of requests that ended because the client disconnected or aborted",
		},
		[]string{"model", "enclave"},
	)

	// SlowHeadersTotal tracks requests where response headers took longer than
	// responseHeaderTimeout to arrive. Observed passively while the request is
	// still in flight; its terminal outcome is counted separately in
	// ProxySuccessTotal or ProxyFailureTotal. Does not trip the breaker.
	SlowHeadersTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "router_slow_headers_total",
			Help: "Total number of requests where response headers took longer than responseHeaderTimeout to arrive",
		},
		[]string{"model", "enclave"},
	)
)
