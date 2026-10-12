package manager

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	verificationTLSProbe = "tls_probe"
	verificationNonce    = "nonce"
	verificationFetch    = "fetch"
	verificationVerify   = "verify"
	verificationKeys     = "keys"
)

var verificationStages = [...]string{verificationTLSProbe, verificationNonce, verificationFetch, verificationVerify, verificationKeys}

type verificationAttempt struct {
	lastAttempt time.Time
	lastFailure time.Time
	lastError   string
	failures    map[string]uint64
}

type backendVerificationStatus struct {
	LastSuccess *time.Time `json:"last_success,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CodeTag     string     `json:"code_tag,omitempty"`
	LastAttempt *time.Time `json:"last_attempt,omitempty"`
	LastFailure *time.Time `json:"last_failure,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	failures    map[string]uint64
}

type modelVerificationStatus struct {
	ObservedAt                 time.Time                            `json:"observed_at"`
	Configured                 int                                  `json:"configured"`
	Verified                   int                                  `json:"verified"`
	Healthy                    int                                  `json:"healthy"`
	Expired                    int                                  `json:"expired"`
	Unverified                 int                                  `json:"unverified"`
	EarliestFreshnessExpiresAt *time.Time                           `json:"earliest_freshness_expires_at,omitempty"`
	Hosts                      map[string]backendVerificationStatus `json:"hosts"`
}

func (em *EnclaveManager) recordVerification(model *Model, host string, attemptedAt time.Time, stage string, err error) {
	model.mu.Lock()
	defer model.mu.Unlock()
	if !slices.Contains(model.configuredHosts, host) {
		return
	}
	if model.verificationAttempts == nil {
		model.verificationAttempts = make(map[string]verificationAttempt)
	}
	attempt := model.verificationAttempts[host]
	attempt.lastAttempt = attemptedAt
	attempt.lastError = ""
	if err != nil {
		if !slices.Contains(verificationStages[:], stage) {
			stage = verificationVerify
		}
		attempt.lastFailure = attemptedAt
		attempt.lastError = stage
		if attempt.failures == nil {
			attempt.failures = make(map[string]uint64)
		}
		attempt.failures[stage]++
	}
	model.verificationAttempts[host] = attempt
}

func verificationTimestamp(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

// verificationStatusLocked reads authenticated state at the observation time;
// local harness connections without attestation do not count as verified.
func (m *Model) verificationStatusLocked(now time.Time) modelVerificationStatus {
	status := modelVerificationStatus{ObservedAt: now.UTC(), Hosts: make(map[string]backendVerificationStatus, len(m.configuredHosts))}
	for _, host := range m.configuredHosts {
		if _, found := status.Hosts[host]; found {
			continue
		}
		status.Configured++
		attempt := m.verificationAttempts[host]
		backend := backendVerificationStatus{
			LastAttempt: verificationTimestamp(attempt.lastAttempt),
			LastFailure: verificationTimestamp(attempt.lastFailure),
			LastError:   attempt.lastError,
			failures:    make(map[string]uint64, len(attempt.failures)),
		}
		for stage, count := range attempt.failures {
			backend.failures[stage] = count
		}
		enclave := m.Enclaves[host]
		var verified *verifiedAttestation
		if enclave != nil && enclave.attestation != nil {
			verified = enclave.attestation.load()
		}
		switch {
		case verified == nil || verified.expiresAt.IsZero():
			status.Unverified++
		default:
			backend.LastSuccess = verificationTimestamp(verified.verifiedAt)
			backend.ExpiresAt = verificationTimestamp(verified.expiresAt)
			backend.CodeTag = verified.codeTag
			if !now.Before(verified.expiresAt) {
				status.Expired++
			} else {
				status.Verified++
				if enclave.cb == nil || enclave.cb.Closed() {
					status.Healthy++
				}
				if status.EarliestFreshnessExpiresAt == nil || verified.expiresAt.Before(*status.EarliestFreshnessExpiresAt) {
					status.EarliestFreshnessExpiresAt = verificationTimestamp(verified.expiresAt)
				}
			}
		}
		status.Hosts[host] = backend
	}
	return status
}

func (m *Model) MarshalJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	type modelJSON Model
	return json.Marshal(struct {
		*modelJSON
		Verification modelVerificationStatus `json:"verification"`
	}{(*modelJSON)(m), m.verificationStatusLocked(time.Now())})
}

type verificationCollector struct {
	manager     *EnclaveManager
	backends    *prometheus.Desc
	lastSuccess *prometheus.Desc
	expiresAt   *prometheus.Desc
	lastAttempt *prometheus.Desc
	failures    *prometheus.Desc
}

// VerificationCollector observes current expiration and breaker state on every
// scrape. Removed hosts disappear without retaining labeled gauge instances.
func (em *EnclaveManager) VerificationCollector() prometheus.Collector {
	return &verificationCollector{
		manager:     em,
		backends:    prometheus.NewDesc("router_verification_backends", "Backend counts by verification state; healthy means unexpired attestation and a closed circuit breaker.", []string{"model", "state"}, nil),
		lastSuccess: prometheus.NewDesc("router_verification_last_success_timestamp_seconds", "Unix timestamp of the last successful attestation, or zero if never verified.", []string{"model", "enclave"}, nil),
		expiresAt:   prometheus.NewDesc("router_verification_expires_timestamp_seconds", "Authenticated attestation expiration as a Unix timestamp, or zero if never verified.", []string{"model", "enclave"}, nil),
		lastAttempt: prometheus.NewDesc("router_verification_last_attempt_timestamp_seconds", "Unix timestamp of the last attestation attempt or failed TLS probe, or zero if never attempted.", []string{"model", "enclave"}, nil),
		failures:    prometheus.NewDesc("router_verification_failures_total", "Failed verification attempts by bounded verification stage.", []string{"model", "enclave", "stage"}, nil),
	}
}

func (c *verificationCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, descriptor := range []*prometheus.Desc{c.backends, c.lastSuccess, c.expiresAt, c.lastAttempt, c.failures} {
		ch <- descriptor
	}
}

func verificationUnixTimestamp(value *time.Time) float64 {
	if value == nil {
		return 0
	}
	return float64(value.Unix())
}

func (c *verificationCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	c.manager.models.Range(func(key, value any) bool {
		model := value.(*Model)
		model.mu.RLock()
		status := model.verificationStatusLocked(now)
		model.mu.RUnlock()
		name := key.(string)
		for state, count := range map[string]int{
			"configured": status.Configured, "verified": status.Verified, "healthy": status.Healthy,
			"expired": status.Expired, "unverified": status.Unverified,
		} {
			ch <- prometheus.MustNewConstMetric(c.backends, prometheus.GaugeValue, float64(count), name, state)
		}
		for host, backend := range status.Hosts {
			ch <- prometheus.MustNewConstMetric(c.lastSuccess, prometheus.GaugeValue, verificationUnixTimestamp(backend.LastSuccess), name, host)
			ch <- prometheus.MustNewConstMetric(c.expiresAt, prometheus.GaugeValue, verificationUnixTimestamp(backend.ExpiresAt), name, host)
			ch <- prometheus.MustNewConstMetric(c.lastAttempt, prometheus.GaugeValue, verificationUnixTimestamp(backend.LastAttempt), name, host)
			for _, stage := range verificationStages {
				ch <- prometheus.MustNewConstMetric(c.failures, prometheus.CounterValue, float64(backend.failures[stage]), name, host, stage)
			}
		}
		return true
	})
}
