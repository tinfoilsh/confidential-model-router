package manager

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/tinfoil-go/verifier/attestation"
	"github.com/tinfoilsh/tinfoil-go/verifier/github"
	"github.com/tinfoilsh/tinfoil-go/verifier/sigstore"

	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/cacheroute"
	"github.com/tinfoilsh/confidential-model-router/config"
	"github.com/tinfoilsh/confidential-model-router/ratelimit"
)

type Enclave struct {
	host      string
	modelName string
	tlsKeyFP  string
	hpkeKey   string
	predicate attestation.PredicateType
	proxy     *httputil.ReverseProxy
	metrics   *enclaveMetrics
	cb        *circuitBreaker

	// inflight counts requests currently proxied through this enclave —
	// a live load signal, unlike the polled queue depth, which can be up
	// to sampleStalenessLimit stale when scrapes fail.
	inflight atomic.Int64
}

type Model struct {
	Repo              string                   `json:"repo"`
	Tag               string                   `json:"tag"`
	SourceMeasurement *attestation.Measurement `json:"measurement"`
	Enclaves          map[string]*Enclave      `json:"enclaves"`
	Overload          *config.OverloadConfig   `json:"overload,omitempty"`
	RateLimit         *config.RateLimitConfig  `json:"rate_limit,omitempty"`
	CacheRoute        *config.CacheRouteConfig `json:"cache_route,omitempty"`
	// Reservations is excluded from JSON: Status() feeds the public
	// /.well-known/tinfoil-proxy endpoint, and org ids must not be
	// exposed there.
	Reservations []config.ReservationConfig `json:"-"`

	expectedHosts int // number of configured hostnames; 0 means no backends expected

	// Derived from Reservations (see applyReservations); replaced wholesale
	// and never mutated after publish, so safe to return under RLock.
	reservedByOrg map[string]map[string]bool // org id -> its reserved host set
	sharedHosts   map[string]bool            // unreserved hosts; nil when no reservations

	mu sync.RWMutex
}

// applyReservations rebuilds the derived reservation lookup sets. Reserved
// hosts not present in the configured hostnames are dropped with a warning:
// counting them would silently shrink an org's effective pool (or leave it
// empty, routing all its traffic to spill). The caller must hold m.mu or
// have exclusive access (addModel, before publish).
func (m *Model) applyReservations(reservations []config.ReservationConfig, hostnames []string) {
	m.Reservations = reservations

	known := make(map[string]bool, len(hostnames))
	for _, host := range hostnames {
		known[host] = true
	}
	byOrg := make(map[string]map[string]bool, len(reservations))
	reserved := make(map[string]bool)
	for _, res := range reservations {
		if len(res.OrgIDs) == 0 || len(res.Enclaves) == 0 {
			continue
		}
		for _, orgID := range res.OrgIDs {
			if orgID == "" {
				continue
			}
			set := byOrg[orgID]
			if set == nil {
				set = make(map[string]bool, len(res.Enclaves))
				byOrg[orgID] = set
			}
			for _, host := range res.Enclaves {
				if !known[host] {
					log.Warnf("reservation for org %s references unknown enclave %s, ignoring", orgID, host)
					continue
				}
				set[host] = true
				reserved[host] = true
			}
			if len(set) == 0 {
				delete(byOrg, orgID)
			}
		}
	}
	if len(byOrg) == 0 {
		m.reservedByOrg = nil
		m.sharedHosts = nil
		return
	}

	shared := make(map[string]bool, len(hostnames))
	for _, host := range hostnames {
		if !reserved[host] {
			shared[host] = true
		}
	}
	m.reservedByOrg = byOrg
	m.sharedHosts = shared
}

// HasReservations reports whether any of this model's enclaves are reserved,
// letting the hot path skip caller-org resolution for unreserved models.
func (m *Model) HasReservations() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.reservedByOrg) > 0
}

// ReservationPools returns the caller's pool filters: primary is the host
// set selection must draw from, spill what it may widen to when the primary
// is exhausted. A reserved org gets its hosts + shared spill; everyone else
// gets the shared hosts with no spill — reserved capacity is exclusive. Both
// nil when the model has no reservations.
func (m *Model) ReservationPools(orgID string) (primary, spill map[string]bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.reservedByOrg) == 0 {
		return nil, nil
	}
	if orgID != "" {
		if hosts, ok := m.reservedByOrg[orgID]; ok {
			return hosts, m.sharedHosts
		}
	}
	return m.sharedHosts, nil
}

type EnclaveManager struct {
	models               *sync.Map // model name -> *Model
	multimodalModels     sync.Map  // sticky set of multimodal chat model names
	initConfigURL        string
	updateConfigURL      string
	controlPlaneURL      string
	sigstoreClient       *sigstore.Client
	billingCollector     *billing.Collector
	usageContextSecret   string
	requestTracker       *ratelimit.RequestTracker
	cacheRouteShadow     *cacheroute.Shadow
	refreshInterval      time.Duration
	stateMu              sync.Mutex
	errors               []string
	lastSuccessfulUpdate time.Time
	lastAttemptedUpdate  time.Time
	debug                bool
}

// SetDebugMode enables debug-only behaviors such as honoring the
// LOCAL_MCP_ENDPOINT_<MODEL> env vars, which point the tool runtime
// at a local, non-attested MCP server. MUST NOT be enabled in
// production enclaves.
func (em *EnclaveManager) SetDebugMode(enabled bool) {
	em.debug = enabled
}

// DebugMode reports whether debug-only overrides are active.
func (em *EnclaveManager) DebugMode() bool {
	return em.debug
}

// ModelExists checks if a model exists
func (em *EnclaveManager) ModelExists(modelName string) bool {
	_, found := em.GetModel(modelName)
	return found
}

// GetModel gets a model by name
func (em *EnclaveManager) GetModel(modelName string) (*Model, bool) {
	model, found := em.models.Load(modelName)
	if !found {
		return nil, false
	}
	return model.(*Model), true
}

// RequestTracker returns the shared request tracker for rate limiting.
func (em *EnclaveManager) RequestTracker() *ratelimit.RequestTracker {
	return em.requestTracker
}

// CacheRouteShadow returns the cache-aware routing shadow tracker.
func (em *EnclaveManager) CacheRouteShadow() *cacheroute.Shadow {
	return em.cacheRouteShadow
}

func (em *EnclaveManager) AddBillingEvent(event billing.Event) {
	if em.billingCollector != nil {
		em.billingCollector.AddEvent(event)
	}
}

// UsageContextSecret returns the HMAC secret used to sign usage-context
// headers attached to outbound MCP tool calls. The empty string means no
// signing should be attempted.
func (em *EnclaveManager) UsageContextSecret() string {
	return em.usageContextSecret
}

// GetRateLimitConfig returns the rate limit config for a model, or nil if not configured.
func (em *EnclaveManager) GetRateLimitConfig(modelName string) *config.RateLimitConfig {
	model, found := em.GetModel(modelName)
	if !found {
		return nil
	}
	model.mu.RLock()
	defer model.mu.RUnlock()
	return model.RateLimit
}

// attestationFetch retrieves the attestation document from a given enclave hostname.
// This is a local implementation that disables HTTP connection pooling to prevent
// certificate validation errors when multiple hostnames (e.g., router.inf4.tinfoil.sh
// and large.inf4.tinfoil.sh) resolve to the same IP address (127.0.0.1:443).
// Without DisableKeepAlives, Go's HTTP/2 client reuses connections, causing SNI mismatches.
func attestationFetch(host string) (*attestation.Document, error) {
	var u url.URL
	u.Host = host
	u.Scheme = "https"
	u.Path = "/.well-known/tinfoil-attestation"

	httpClient := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true, // Prevents connection reuse across different hostnames
		},
	}
	resp, err := httpClient.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var doc attestation.Document
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// addEnclave verifies and adds an enclave to the model enclave pool.
// If the enclave already exists, replace it if the TLS key fingerprint is different.
func (em *EnclaveManager) addEnclave(
	modelName, host string,
	hwMeasurements []*attestation.HardwareMeasurement,
) error {
	model, found := em.GetModel(modelName)
	if !found {
		return fmt.Errorf("model %s not found", modelName)
	}

	model.mu.Lock()
	defer model.mu.Unlock()

	// If the enclave already exists and the TLS key fingerprint is the same, do nothing
	currentEnclave, exists := model.Enclaves[host]
	if exists {
		realTLSKeyFP, err := attestation.TLSPublicKey(host, false)
		if err == nil && currentEnclave.tlsKeyFP == realTLSKeyFP {
			log.Debugf("enclave %s already exists and TLS key fingerprint is the same, skipping", host)
			return nil
		}
	}

	remoteAttestation, err := attestationFetch(host)
	if err != nil {
		return fmt.Errorf("failed to fetch remote attestation: %v", err)
	}
	verification, err := remoteAttestation.Verify()
	if err != nil {
		return fmt.Errorf("failed to verify remote attestation: %v", err)
	}
	if verification.Measurement.Type == attestation.TdxGuestV2 {
		_, err = attestation.VerifyHardware(hwMeasurements, verification.Measurement)
		if err != nil {
			return fmt.Errorf("failed to verify hardware measurements: %v", err)
		}
	}

	// Validate that the enclave's attested measurement matches the model's source measurement
	// SECURITY: This check is critical - it ensures the enclave runs the expected code
	if model.SourceMeasurement == nil {
		return fmt.Errorf("cannot add enclave %s: source measurement not available", host)
	}
	if err := verification.Measurement.Equals(model.SourceMeasurement); err != nil {
		return fmt.Errorf("measurement mismatch for enclave %s: %v", host, err)
	}

	cb := newCircuitBreaker()
	model.Enclaves[host] = &Enclave{
		host:      host,
		modelName: modelName,
		predicate: verification.Measurement.Type,
		tlsKeyFP:  verification.TLSPublicKeyFP,
		hpkeKey:   verification.HPKEPublicKey,
		proxy:     newProxy(host, verification.TLSPublicKeyFP, modelName, em.billingCollector, cb),
		metrics:   newEnclaveMetrics(host, modelName),
		cb:        cb,
	}
	model.Enclaves[host].updateOverloadConfig(model.Overload)
	CircuitBreakerState.WithLabelValues(modelName, host).Set(float64(cbClosed))
	return nil
}

// Models returns all models
func (em *EnclaveManager) Models() map[string]*Model {
	models := make(map[string]*Model)
	em.models.Range(func(key, value any) bool {
		models[key.(string)] = value.(*Model)
		return true
	})
	return models
}

// Ready returns true if the router has completed at least one successful sync
// and is able to serve requests.
func (em *EnclaveManager) Ready() bool {
	em.stateMu.Lock()
	defer em.stateMu.Unlock()
	return !em.lastSuccessfulUpdate.IsZero()
}

// Status returns the status of the enclave manager to be JSON encoded
func (em *EnclaveManager) Status() map[string]any {
	em.stateMu.Lock()
	errors := make([]string, len(em.errors))
	copy(errors, em.errors)
	updated := em.lastSuccessfulUpdate
	attempted := em.lastAttemptedUpdate
	em.stateMu.Unlock()

	return map[string]any{
		"models":    em.Models(),
		"errors":    errors,
		"updated":   updated,
		"attempted": attempted,
	}
}

// PrometheusTargetGroup represents a Prometheus HTTP SD target group
type PrometheusTargetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// PrometheusTargets returns targets in Prometheus HTTP service discovery format
// See: https://prometheus.io/docs/prometheus/latest/configuration/configuration/#http_sd_config
func (em *EnclaveManager) PrometheusTargets() []PrometheusTargetGroup {
	var targetGroups []PrometheusTargetGroup

	em.models.Range(func(key, value any) bool {
		modelName := key.(string)
		model := value.(*Model)

		model.mu.RLock()
		defer model.mu.RUnlock()

		if len(model.Enclaves) == 0 {
			return true
		}

		targets := make([]string, 0, len(model.Enclaves))
		for host := range model.Enclaves {
			targets = append(targets, host)
		}

		targetGroups = append(targetGroups, PrometheusTargetGroup{
			Targets: targets,
			Labels: map[string]string{
				"model_name":    modelName,
				"__param_model": modelName,
			},
		})

		return true
	})

	return targetGroups
}

// Shutdown gracefully stops the billing collector
func (em *EnclaveManager) Shutdown() {
	if em.billingCollector != nil {
		em.billingCollector.Stop()
	}
	if em.cacheRouteShadow != nil {
		em.cacheRouteShadow.Close()
	}
	em.models.Range(func(_, value any) bool {
		model := value.(*Model)
		model.mu.RLock()
		for _, enclave := range model.Enclaves {
			enclave.shutdown()
		}
		model.mu.RUnlock()
		return true
	})
}

// claimProbe attempts to claim the enclave's due recovery probe, returning
// the claim the dispatched request must carry, or nil when no probe is due.
func (e *Enclave) claimProbe() *ProbeClaim {
	if e.cb == nil {
		return nil
	}
	token, ok := e.cb.ClaimProbe()
	if !ok {
		return nil
	}
	return &ProbeClaim{cb: e.cb, token: token, modelName: e.modelName, host: e.host}
}

// NextEnclave picks a uniformly random enclave with a closed circuit breaker,
// preferring those that are not currently overloaded and not in skip. If an
// open breaker is past its cooldown, it sends a probe to that enclave. Falls
// back to any closed enclave, then any enclave (degraded > unavailable).
// A non-nil ProbeClaim means the pick is a claimed recovery probe: the
// request dispatched to it must carry the claim (see ProbeClaim).
func (m *Model) NextEnclave(skip map[string]bool) (*Enclave, *ProbeClaim) {
	return m.nextEnclave(skip, true, nil)
}

// nextEnclave is NextEnclave with probe claiming optional and an optional
// allowed host filter (nil = all hosts). The filter is hard: it applies to
// every bucket including the degraded last resort and probe claiming.
func (m *Model) nextEnclave(skip map[string]bool, claimProbes bool, allowed map[string]bool) (*Enclave, *ProbeClaim) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	all := make([]*Enclave, 0, len(m.Enclaves))
	var closed, preferred []*Enclave
	for _, enclave := range m.Enclaves {
		if allowed != nil && !allowed[enclave.host] {
			continue
		}
		all = append(all, enclave)
		if enclave.cb != nil && !enclave.cb.Closed() {
			if claimProbes {
				if claim := enclave.claimProbe(); claim != nil {
					return enclave, claim
				}
			}
			continue
		}
		if skip[enclave.host] {
			continue
		}
		closed = append(closed, enclave)
		if !enclave.isOverloaded() {
			preferred = append(preferred, enclave)
		}
	}

	switch {
	case len(preferred) > 0:
		return preferred[rand.IntN(len(preferred))], nil
	case len(closed) > 0:
		return closed[rand.IntN(len(closed))], nil
	case len(all) > 0:
		return all[rand.IntN(len(all))], nil
	}
	return nil, nil
}

// NextEnclavePreferring picks like NextEnclave, but serves the first
// acceptable host from order — the cache-aware preference, best first —
// before falling back to random selection. Health still beats cache: a due
// probe wins, and order hosts are honored only while their breakers are
// closed. The overload flag deliberately does not veto an order host; every
// caller must step aside per request via ShouldReject and skip (main.go's
// retry loop, SelectForDispatch for internal dispatches), so a flapping
// load signal can never move a key's home.
func (m *Model) NextEnclavePreferring(order []string, skip map[string]bool) (*Enclave, *ProbeClaim) {
	return m.nextEnclavePreferring(order, skip, true, nil)
}

func (m *Model) nextEnclavePreferring(order []string, skip map[string]bool, claimProbes bool, allowed map[string]bool) (*Enclave, *ProbeClaim) {
	if len(order) == 0 {
		return m.nextEnclave(skip, claimProbes, allowed)
	}

	if claimProbes {
		m.mu.RLock()
		for _, enclave := range m.Enclaves {
			if allowed != nil && !allowed[enclave.host] {
				continue
			}
			if enclave.cb != nil && !enclave.cb.Closed() {
				if claim := enclave.claimProbe(); claim != nil {
					m.mu.RUnlock()
					return enclave, claim
				}
			}
		}
		m.mu.RUnlock()
	}
	m.mu.RLock()
	for _, host := range order {
		if allowed != nil && !allowed[host] {
			continue
		}
		enclave := m.Enclaves[host]
		if enclave == nil || skip[host] {
			continue
		}
		if enclave.cb != nil && !enclave.cb.Closed() {
			continue
		}
		m.mu.RUnlock()
		return enclave, nil
	}
	m.mu.RUnlock()
	return m.nextEnclave(skip, claimProbes, allowed)
}

// SelectForDispatch picks the serving enclave for an internal dispatch (the
// tool loop), honoring the cache-aware order with the same overload spill as
// the public retry loop: an overloaded pick is skipped and selection moves
// to the next-warmest host. Internal dispatches have no 429 path, so when
// every live candidate is overloaded it serves through overload at the
// warmest one — never at a breaker-open host, which is reachable only when
// no closed replica exists at all. Internal dispatches record their outcomes,
// so they may also claim a due recovery probe.
func (m *Model) SelectForDispatch(order []string) (*Enclave, *ProbeClaim) {
	return m.selectForDispatch(order, true, nil)
}

// SelectForDispatchPools picks like SelectForDispatch over the caller's
// reservation pools: primary first, spill only when every primary candidate
// is overloaded or unhealthy. Both exhausted serves through overload at the
// primary pick (no-429 contract).
func (m *Model) SelectForDispatchPools(order []string, primary, spill map[string]bool) (*Enclave, *ProbeClaim) {
	return m.selectForDispatchPools(order, true, primary, spill)
}

func (m *Model) selectForDispatchPools(order []string, claimProbes bool, primary, spill map[string]bool) (*Enclave, *ProbeClaim) {
	enclave, claim := m.selectForDispatch(order, claimProbes, primary)
	// A claimed probe must be dispatched.
	if len(spill) == 0 || claim != nil {
		return enclave, claim
	}
	if enclave != nil && enclave.breakerClosed() {
		if overloaded, _, _ := enclave.ShouldReject(); !overloaded {
			return enclave, nil
		}
	}
	spillEnclave, spillClaim := m.selectForDispatch(order, claimProbes, spill)
	if spillEnclave != nil {
		if spillClaim != nil {
			return spillEnclave, spillClaim
		}
		if spillEnclave.breakerClosed() {
			if overloaded, _, _ := spillEnclave.ShouldReject(); !overloaded {
				return spillEnclave, nil
			}
		}
	}
	// Both pools exhausted: serve through overload at the primary pick.
	if enclave != nil {
		return enclave, nil
	}
	return spillEnclave, nil
}

// selectForDispatch is SelectForDispatch with probe claiming optional, for
// callers that hand the connection to a transport that records no breaker
// outcomes (a claimed probe there would strand the breaker half-open), and
// an optional allowed host filter (see nextEnclave).
func (m *Model) selectForDispatch(order []string, claimProbes bool, allowed map[string]bool) (*Enclave, *ProbeClaim) {
	skip := map[string]bool{}
	var firstOverloaded *Enclave
	var enclave *Enclave
	for range m.EnclaveCount() {
		var claim *ProbeClaim
		enclave, claim = m.nextEnclavePreferring(order, skip, claimProbes, allowed)
		if enclave == nil {
			break
		}
		// A claimed probe must be dispatched: preferring an overloaded
		// pick over it would leave the claim unresolved.
		if claim != nil {
			return enclave, claim
		}
		if enclave.cb != nil && !enclave.cb.Closed() {
			// Selection is down to non-closed last resorts: every
			// remaining closed candidate is overloaded or none exist.
			break
		}
		if overloaded, _, _ := enclave.ShouldReject(); !overloaded {
			return enclave, nil
		}
		if firstOverloaded == nil {
			firstOverloaded = enclave
		}
		skip[enclave.String()] = true
	}
	if firstOverloaded != nil {
		return firstOverloaded, nil
	}
	return enclave, nil
}

// SelectServing picks the serving enclave for a public proxied request: the
// overload skip loop over the caller's primary pool, widening to spill only
// when every primary candidate is overloaded or unhealthy (nil primary =
// all enclaves, no reservations). A nil enclave means 503; overloaded=true
// is the 429 verdict with retryAfter/waiting; a non-nil ProbeClaim must
// travel with the dispatched request.
func (m *Model) SelectServing(order []string, primary, spill map[string]bool) (enclave *Enclave, claim *ProbeClaim, overloaded bool, retryAfter time.Duration, waiting float64) {
	enclave, claim, overloaded, retryAfter, waiting = m.selectServingFrom(order, primary)
	// A claimed probe must be dispatched. Breaker-open last resorts do
	// spill: an all-dead reserved pool should fail over to shared capacity,
	// not serve at a dead host.
	if len(spill) == 0 || claim != nil {
		return
	}
	if enclave != nil && !overloaded && enclave.breakerClosed() {
		return
	}
	spillEnclave, spillClaim, spillOverloaded, spillRetryAfter, spillWaiting := m.selectServingFrom(order, spill)
	if spillEnclave == nil {
		return
	}
	if enclave == nil || spillClaim != nil || (!spillOverloaded && spillEnclave.breakerClosed()) {
		return spillEnclave, spillClaim, spillOverloaded, spillRetryAfter, spillWaiting
	}
	// Both pools exhausted: surface the primary pool's overload verdict.
	return
}

func (m *Model) selectServingFrom(order []string, allowed map[string]bool) (enclave *Enclave, claim *ProbeClaim, overloaded bool, retryAfter time.Duration, waiting float64) {
	skip := map[string]bool{}
	for range m.EnclaveCount() {
		enclave, claim = m.nextEnclavePreferring(order, skip, true, allowed)
		if enclave == nil {
			return
		}
		// A claimed probe must be dispatched or the breaker strands
		// half-open; overload skip applies to plain picks only.
		if claim != nil {
			overloaded = false
			return
		}
		if overloaded, retryAfter, waiting = enclave.ShouldReject(); !overloaded {
			return
		}
		skip[enclave.String()] = true
	}
	return
}

// EnclaveCount returns the number of configured enclaves under the model lock.
// Callers that need to bound a retry loop on enclave cardinality should use
// this rather than reading len(m.Enclaves) directly.
func (m *Model) EnclaveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Enclaves)
}

// CacheRoutePool snapshots the model's replica set for cache-aware routing:
// breaker-closed enclaves with live in-flight counts, falling back to all
// enclaves when every breaker is open (mirroring NextEnclave). The overload
// flag is deliberately ignored — membership must stay stable so a flapping
// load signal can't move a key's home.
func (m *Model) CacheRoutePool() cacheroute.Pool {
	return m.CacheRoutePoolIn(nil)
}

// CacheRoutePoolIn is CacheRoutePool restricted to the caller's reservation
// pool (nil = all), keeping HRW ranking — and a key's warm home — inside it.
func (m *Model) CacheRoutePoolIn(allowed map[string]bool) cacheroute.Pool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Size comes from the config, not the live enclave map: during a
	// release the map is cleared and re-attested one host at a time, and a
	// multi-replica pool must not reclassify as pool_too_small (skipping
	// warmth tracking) for that window. A restricted pool's size is the
	// configured size of that pool for the same reason.
	size := m.expectedHosts
	if allowed != nil {
		size = len(allowed)
	}
	pool := cacheroute.Pool{Size: size}
	for _, enclave := range m.Enclaves {
		if allowed != nil && !allowed[enclave.host] {
			continue
		}
		if enclave.cb != nil && !enclave.cb.Closed() {
			continue
		}
		pool.Candidates = append(pool.Candidates, cacheroute.Candidate{
			Host:     enclave.host,
			InFlight: int(enclave.inflight.Load()),
		})
	}
	if len(pool.Candidates) == 0 {
		for _, enclave := range m.Enclaves {
			if allowed != nil && !allowed[enclave.host] {
				continue
			}
			pool.Candidates = append(pool.Candidates, cacheroute.Candidate{
				Host:     enclave.host,
				InFlight: int(enclave.inflight.Load()),
			})
		}
	}
	return pool
}

// CacheRouteSettings returns the model's resolved cache-route settings.
func (m *Model) CacheRouteSettings() cacheroute.Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cacheroute.SettingsFrom(m.CacheRoute)
}

// HasHealthyEnclave reports whether the model has at least one enclave whose
// circuit breaker is currently closed. Used by ResolvePreferredModel to skip
// models whose backends are all tripped (i.e. effectively down).
func (m *Model) HasHealthyEnclave() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, enclave := range m.Enclaves {
		if enclave.cb == nil || enclave.cb.Closed() {
			return true
		}
	}
	return false
}

// ResolvePreferredModel returns the first candidate whose model exists and has
// a healthy enclave. When none are healthy it falls back to the first non-empty
// candidate (so normal serving surfaces the error), and returns "" when the
// list is empty. The result is therefore preferred, not guaranteed healthy.
func (em *EnclaveManager) ResolvePreferredModel(candidates []string) string {
	var first string
	for _, name := range candidates {
		if name == "" {
			continue
		}
		if first == "" {
			first = name
		}
		if model, found := em.GetModel(name); found && model.HasHealthyEnclave() {
			return name
		}
	}
	return first
}

func (e *Enclave) isOverloaded() bool {
	return e != nil && e.metrics != nil && e.metrics.overloaded.Load()
}

// breakerClosed reports whether the circuit breaker is closed (nil counts
// as closed, matching selection's treatment).
func (e *Enclave) breakerClosed() bool {
	return e.cb == nil || e.cb.Closed()
}

func (e *Enclave) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.inflight.Add(1)
	defer e.inflight.Add(-1)

	w.Header().Set("Tinfoil-Enclave", e.host)

	// Check if client requested usage metrics in response header/trailer
	usageMetricsRequested := r.Header.Get(UsageMetricsRequestHeader) == "true"
	if !usageMetricsRequested {
		e.proxy.ServeHTTP(w, r)
		return
	}

	// Wrap the ResponseWriter to capture usage and write trailer
	wrapper := &usageMetricsWriter{ResponseWriter: w, model: e.modelName}

	// Store wrapper in request context for ModifyResponse to access
	ctx := context.WithValue(r.Context(), usageWriterKey{}, wrapper)

	e.proxy.ServeHTTP(wrapper, r.WithContext(ctx))

	// Write the trailer after body completes (for streaming responses)
	// For non-streaming, the header was already set in ModifyResponse
	if wrapper.TrailerEnabled() {
		wrapper.WriteTrailer()
	}
}

func (e *Enclave) MarshalJSON() ([]byte, error) {
	fields := map[string]string{
		"predicate":  string(e.predicate),
		"tls_key_fp": e.tlsKeyFP,
	}
	if e.hpkeKey != "" {
		fields["hpke_key"] = e.hpkeKey
	}
	return json.Marshal(fields)
}

func (e *Enclave) UnmarshalJSON(data []byte) error {
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	e.predicate = attestation.PredicateType(m["predicate"])
	e.tlsKeyFP = m["tls_key_fp"]
	e.hpkeKey = m["hpke_key"]
	return nil
}

func (e *Enclave) String() string {
	return e.host
}

// NewEnclaveManager loads model repos from the local config file (not remote) into the enclave manager
func NewEnclaveManager(configFile []byte, controlPlaneURL string, usageReporterID string, usageReporterSecret string, usageContextSecret string, initConfigURL string, updateConfigURL string, refreshInterval time.Duration) (*EnclaveManager, error) {
	if refreshInterval <= 0 {
		return nil, fmt.Errorf("refresh interval must be positive, got %v", refreshInterval)
	}

	var cfg *config.Config
	var err error
	if initConfigURL != "" {
		cfg, err = config.Load(initConfigURL, true)
	} else {
		cfg, err = config.FromBytes(configFile)
	}
	if err != nil {
		return nil, err
	}

	sigstoreClient, err := sigstore.NewClient()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch trust root: %v", err)
	}

	em := &EnclaveManager{
		models:             &sync.Map{},
		initConfigURL:      initConfigURL,
		updateConfigURL:    updateConfigURL,
		controlPlaneURL:    controlPlaneURL,
		sigstoreClient:     sigstoreClient,
		billingCollector:   billing.NewCollector(controlPlaneURL, usageReporterID, usageReporterSecret),
		usageContextSecret: usageContextSecret,
		requestTracker:     ratelimit.NewRequestTracker(),
		cacheRouteShadow:   cacheroute.NewShadow(nil),
		refreshInterval:    refreshInterval,
	}

	for modelName, modelConfig := range cfg.Models {
		em.addModel(modelName, modelConfig)
	}

	log.Infof("Loaded %d model(s) from initial config", len(cfg.Models))

	return em, nil
}

func (em *EnclaveManager) addModel(modelName string, modelConfig config.Model) {
	model := &Model{
		Repo:              modelConfig.Repo,
		Tag:               "",
		SourceMeasurement: nil,
		Enclaves:          make(map[string]*Enclave),
		Overload:          modelConfig.Overload,
		RateLimit:         modelConfig.RateLimit,
		CacheRoute:        modelConfig.CacheRoute,
		expectedHosts:     len(modelConfig.Hostnames),
	}
	model.applyReservations(modelConfig.Reservations, modelConfig.Hostnames)
	em.models.Store(modelName, model)
	cacheroute.SetMode(modelName, modelConfig.CacheRoute)
	cacheroute.SetPoolInfo(modelName, modelConfig.Hostnames)
}

// updateModelMeasurements checks if there's a new tag, and if so, updates the model's tag and measurement
func (em *EnclaveManager) updateModelMeasurements(modelName string) (bool, error) {
	model, found := em.GetModel(modelName)
	if !found {
		return false, fmt.Errorf("model %s not found", modelName)
	}

	log.Tracef("updating model measurements for %s", modelName)

	latestTag, err := github.FetchLatestTag(model.Repo)
	if err != nil {
		return false, fmt.Errorf("failed to fetch latest tag: %v", err)
	}

	if model.Tag == latestTag {
		return false, nil
	}

	digest, err := github.FetchDigest(model.Repo, latestTag)
	if err != nil {
		return false, fmt.Errorf("failed to fetch latest release for %s@%s: %v", model.Repo, latestTag, err)
	}
	sigstoreBundle, err := github.FetchAttestationBundle(model.Repo, digest)
	if err != nil {
		return false, fmt.Errorf("failed to fetch attestation bundle: %v", err)
	}
	measurement, err := em.sigstoreClient.VerifyAttestation(sigstoreBundle, model.Repo, digest)
	if err != nil {
		return false, fmt.Errorf("failed to verify attestation: %v", err)
	}

	model.mu.Lock()
	defer model.mu.Unlock()

	model.Tag = latestTag
	model.SourceMeasurement = measurement
	for _, enclave := range model.Enclaves {
		enclave.shutdown()
	}
	model.Enclaves = make(map[string]*Enclave) // Clear all enclaves, their measurements are now invalid

	return true, nil
}

// sync updates all model's tags and measurements, then matches them to the enclave config
func (em *EnclaveManager) sync() error {
	log.Debug("Updating all models")
	em.stateMu.Lock()
	em.lastAttemptedUpdate = time.Now()
	em.stateMu.Unlock()

	config, err := config.Load(em.updateConfigURL, false)
	if err != nil {
		return fmt.Errorf("failed to fetch config: %v", err)
	}

	em.refreshMultimodalModels()

	// Fetch hardware measurements
	hwMeasurements, err := em.sigstoreClient.LatestHardwareMeasurements()
	if err != nil {
		return fmt.Errorf("failed to fetch hardware measurements: %v", err)
	}

	var wg sync.WaitGroup
	em.models.Range(func(key, value any) bool {
		wg.Add(1)
		go func() {
			defer wg.Done()
			modelName := key.(string)
			model := value.(*Model)

			configModel, modelInConfig := config.Models[modelName]
			if !modelInConfig {
				log.Warnf("model %s no longer in config", modelName)
				model.mu.Lock()
				model.expectedHosts = 0
				model.applyReservations(nil, nil)
				for _, enclave := range model.Enclaves {
					enclave.shutdown()
				}
				model.Enclaves = make(map[string]*Enclave)
				model.mu.Unlock()
				cacheroute.DropModel(modelName)
				return
			}

			// If the repo has changed, display a warning.
			if model.Repo != configModel.Repo {
				log.Errorf("repo changed for model %s: %s -> %s. Publish a new release and update this router if you want the change to take effect.", modelName, model.Repo, configModel.Repo)
				return
			}

			_, err := em.updateModelMeasurements(modelName)
			if err != nil {
				log.Errorf("failed to update model measurements for %s: %v", modelName, err)
			}

			log.Tracef("Updating config for model %s", modelName)
			model.mu.Lock()
			model.expectedHosts = len(configModel.Hostnames)
			model.Overload = configModel.Overload
			model.RateLimit = configModel.RateLimit
			model.CacheRoute = configModel.CacheRoute
			model.applyReservations(configModel.Reservations, configModel.Hostnames)
			for _, enclave := range model.Enclaves {
				enclave.updateOverloadConfig(model.Overload)
			}
			cacheroute.SetMode(modelName, configModel.CacheRoute)
			cacheroute.SetPoolInfo(modelName, configModel.Hostnames)

			// Updating enclaves for each model
			log.Tracef("updating enclaves for model %s", modelName)
			hostnames := configModel.Hostnames

			// Remove enclaves that are no longer in the config
			for existingHost := range model.Enclaves {
				if !slices.Contains(hostnames, existingHost) {
					log.Warnf("hostname %s no longer in config, removing", existingHost)
					model.Enclaves[existingHost].shutdown()
					delete(model.Enclaves, existingHost)
				}
			}
			model.mu.Unlock()

			// Add new enclave from the config and update attestation if needed
			for _, host := range hostnames {
				log.Tracef("  + host %s", host)
				if err := em.addEnclave(modelName, host, hwMeasurements); err != nil {
					em.stateMu.Lock()
					em.errors = append(em.errors, err.Error())
					em.stateMu.Unlock()
					log.Errorf("failed to add enclave %s for model %s: %v", host, modelName, err)
				}
			}
		}()
		return true
	})
	wg.Wait()

	em.stateMu.Lock()
	em.lastSuccessfulUpdate = time.Now()
	em.stateMu.Unlock()

	return nil
}

// StartWorker starts the worker update loop
func (em *EnclaveManager) StartWorker() {
	ticker := time.NewTicker(em.refreshInterval)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		em.stateMu.Lock()
		em.errors = []string{} // Clear errors
		em.stateMu.Unlock()
		if err := em.sync(); err != nil {
			log.Errorf("failed to update: %v", err)
			em.stateMu.Lock()
			em.errors = append(em.errors, err.Error())
			em.stateMu.Unlock()
		}
	}
}

func (e *Enclave) updateOverloadConfig(cfg *config.OverloadConfig) {
	if e == nil || e.metrics == nil {
		return
	}
	e.metrics.setConfig(cfg)
}

// shutdown stops the enclave's background polling and drops its per-enclave
// gauge series. Gauges assert current state, so a series left behind for a
// removed host keeps reporting stale queue depth, overload, and breaker
// state until process restart — overcounting any pool-size query after a
// resize. Counters stay: their history remains truthful and rate() of a
// flat counter is zero. The breaker is retired first so callbacks from
// requests still draining cannot republish the deleted gauge.
func (e *Enclave) shutdown() {
	if e == nil {
		return
	}
	if e.metrics != nil {
		e.metrics.shutdown()
	}
	if e.cb != nil {
		e.cb.Retire()
	}
	BackendQueueDepth.DeleteLabelValues(e.modelName, e.host)
	BackendOverloaded.DeleteLabelValues(e.modelName, e.host)
	CircuitBreakerState.DeleteLabelValues(e.modelName, e.host)
}

func (e *Enclave) ShouldReject() (bool, time.Duration, float64) {
	if e == nil || e.metrics == nil {
		return false, 0, 0
	}
	// A pick whose breaker isn't closed is a recovery probe or a last
	// resort — rejecting it for overload defeats its purpose (and swallows
	// the probe: claimed but never dispatched, the breaker would strand in
	// half-open). Its queue metrics are suspect while the host fails, too.
	if e.cb != nil && !e.cb.Closed() {
		return false, 0, 0
	}
	return e.metrics.shouldReject()
}

// OverloadMarks returns the resolved overload trip and clear marks for this
// enclave, or ok=false when no usable overload thresholds are configured.
func (e *Enclave) OverloadMarks() (trip, clear int, ok bool) {
	if e == nil || e.metrics == nil {
		return 0, 0, false
	}
	cfg := e.metrics.currentConfig()
	if cfg == nil || cfg.MaxRequestsWaiting <= 0 {
		return 0, 0, false
	}
	trip, clear = cfg.Marks()
	return trip, clear, true
}
