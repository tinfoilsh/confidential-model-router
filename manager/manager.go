package manager

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/tinfoil-go"
	"github.com/tinfoilsh/tinfoil-go/verify/measurement"

	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/cacheroute"
	"github.com/tinfoilsh/confidential-model-router/config"
	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

type Enclave struct {
	host      string
	modelName string
	client    enclaveClient
	proxy     *httputil.ReverseProxy
	metrics   *enclaveMetrics
	cb        *circuitBreaker
	pricing   func(string) (ModelPricing, bool)

	// inflight counts requests selected for or served through this enclave —
	// a live load signal, unlike the polled queue depth, which can be up
	// to sampleStalenessLimit stale when scrapes fail.
	inflight atomic.Int64
}

type Model struct {
	Repo              string                   `json:"repo"`
	Project           string                   `json:"project,omitempty"`
	Tag               string                   `json:"tag"`
	SourceMeasurement measurement.Expected     `json:"measurement"`
	Enclaves          map[string]*Enclave      `json:"enclaves"`
	Overload          *config.OverloadConfig   `json:"overload,omitempty"`
	CacheRoute        *config.CacheRouteConfig `json:"cache_route,omitempty"`
	// Reservations is excluded from JSON: Status() feeds the public
	// /.well-known/tinfoil-proxy endpoint, and org and user ids must not be
	// exposed there.
	Reservations []config.ReservationConfig `json:"-"`

	expectedHosts   int // number of configured hostnames; 0 means no backends expected
	configuredHosts map[string]verificationAttempt

	// Derived from Reservations (see applyReservations); replaced wholesale
	// and never mutated after publish, so safe to return under RLock.
	reservedByCaller     map[Caller]map[string]bool // org or user -> its reserved host set
	sharedHosts          map[string]bool            // unreserved hosts; nil when no reservations
	reservationOverloads map[string]*config.OverloadConfig
	policiesByCaller     map[Caller]*config.OverloadConfig

	mu sync.RWMutex
}

// applyReservations rebuilds the derived reservation lookup sets. Reserved
// hosts not present in the configured hostnames are dropped with a warning:
// counting them would silently shrink a caller's effective pool (or leave it
// empty, routing all its traffic to spill). The caller must hold m.mu or
// have exclusive access (addModel, before publish).
func (m *Model) applyReservations(name string, reservations []config.ReservationConfig, hostnames []string) {
	m.Reservations = reservations

	known := make(map[string]bool, len(hostnames))
	for _, host := range hostnames {
		known[host] = true
	}
	byCaller := make(map[Caller]map[string]bool, len(reservations))
	reserved := make(map[string]bool)
	overloads := make(map[string]*config.OverloadConfig)
	policies := make(map[Caller]*config.OverloadConfig)
	for _, res := range reservations {
		if len(res.Enclaves) == 0 {
			continue
		}
		callers := make([]Caller, 0, len(res.OrgIDs)+len(res.UserIDs))
		for _, orgID := range res.OrgIDs {
			callers = append(callers, Caller{OrgID: orgID})
		}
		for _, userID := range res.UserIDs {
			callers = append(callers, Caller{UserID: userID})
		}
		for _, caller := range callers {
			if caller == (Caller{}) {
				continue
			}
			policy := res.Overload
			if policy == nil {
				policy = m.Overload
			}
			set := byCaller[caller]
			if set == nil {
				set = make(map[string]bool, len(res.Enclaves))
				byCaller[caller] = set
			}
			recognized := false
			for _, host := range res.Enclaves {
				if !known[host] {
					log.WithFields(log.Fields{"org_id": caller.OrgID, "user_id": caller.UserID}).Warnf("reservation references unknown enclave %s, ignoring", host)
					continue
				}
				set[host] = true
				reserved[host] = true
				overloads[host] = res.Overload
				recognized = true
			}
			if len(set) == 0 {
				delete(byCaller, caller)
				continue
			}
			if recognized {
				policies[caller] = policy
			}
		}
	}
	ReservedPoolSize.WithLabelValues(name).Set(float64(len(reserved)))
	// Add before deleting, and delete only hosts that left the pool, so a
	// scrape mid-reload may over-list reserved hosts but never miss one.
	for host := range reserved {
		ReservedEnclave.WithLabelValues(name, host).Set(1)
	}
	for host := range m.reservationOverloads {
		if !reserved[host] {
			ReservedEnclave.DeleteLabelValues(name, host)
		}
	}
	m.reservationOverloads = overloads
	m.policiesByCaller = policies
	if len(byCaller) == 0 {
		m.reservedByCaller = nil
		m.sharedHosts = nil
		return
	}

	shared := make(map[string]bool, len(hostnames))
	for _, host := range hostnames {
		if !reserved[host] {
			shared[host] = true
		}
	}
	m.reservedByCaller = byCaller
	m.sharedHosts = shared
}

// overloadForHostLocked resolves the backend policy while m.mu is held.
func (m *Model) overloadForHostLocked(host string) *config.OverloadConfig {
	if override := m.reservationOverloads[host]; override != nil {
		return override
	}
	return m.Overload
}

// HasReservations reports whether any of this model's enclaves are reserved,
// letting the hot path skip caller resolution for unreserved models.
func (m *Model) HasReservations() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.reservedByCaller) > 0
}

// ReservationPools snapshots the caller's pool filters and policy. Reserved
// callers get their hosts plus shared spillover; everyone else gets shared
// hosts only. A user reservation takes precedence over an org reservation.
// Both filters are nil when the model has no reservations.
func (m *Model) ReservationPools(caller Caller) (primary, spill map[string]bool, policy *config.OverloadConfig) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.reservedByCaller) == 0 {
		return nil, nil, m.Overload
	}
	for _, subject := range []Caller{{UserID: caller.UserID}, {OrgID: caller.OrgID}} {
		if hosts, ok := m.reservedByCaller[subject]; ok {
			return hosts, m.sharedHosts, m.policiesByCaller[subject]
		}
	}
	return m.sharedHosts, nil, m.Overload
}

type EnclaveManager struct {
	models               *sync.Map // model name -> *Model
	multimodalModels     sync.Map  // sticky set of multimodal chat model names
	modelPricing         atomic.Pointer[map[string]ModelPricing]
	modelIntelligence    atomic.Pointer[map[string]ModelIntelligence]
	initConfigURL        string
	updateConfigURL      string
	controlPlaneURL      string
	connect              func(ctx context.Context, host string, pin tinfoil.Pin) (enclaveClient, error)
	billingCollector     *billing.Collector
	unknownModelReporter *billing.UnknownModelReporter
	usageContextSecret   string
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

// GetModel gets a model by name
func (em *EnclaveManager) GetModel(modelName string) (*Model, bool) {
	model, found := em.models.Load(modelName)
	if !found {
		return nil, false
	}
	return model.(*Model), true
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

// ReportUnknownModel tells the controlplane that an authenticated request
// was rejected because modelName is not served.
func (em *EnclaveManager) ReportUnknownModel(apiKey, modelName string) {
	em.unknownModelReporter.Report(apiKey, modelName)
}

// UsageContextSecret returns the HMAC secret used to verify the usage context
// the chat harness sends and to sign usage-context headers attached to
// outbound MCP tool calls. The empty string means no signing should be
// attempted.
func (em *EnclaveManager) UsageContextSecret() string {
	return em.usageContextSecret
}

// addEnclave verifies host and adds it to the model's pool. An enclave that
// is already there keeps its client, breaker and metrics, and is verified
// again once its renewal is due.
func (em *EnclaveManager) addEnclave(modelName, host string) (err error) {
	model, found := em.GetModel(modelName)
	if !found {
		return fmt.Errorf("model %s not found", modelName)
	}

	// Don't hold the model lock during the network I/O below.
	model.mu.RLock()
	enclave := model.Enclaves[host]
	model.mu.RUnlock()

	var client enclaveClient
	var verified attestation
	if enclave != nil {
		client = enclave.client
		verified = client.current()
	}
	if enclave == nil || verified.refreshDue(time.Now(), em.refreshInterval) {
		attemptedAt := time.Now()
		defer func() { em.recordVerification(model, host, attemptedAt, err) }()
		ctx, cancel := context.WithTimeout(context.Background(), enclaveVerifyTimeout)
		defer cancel()
		// The pinned workload and signed collateral freshness define accepted releases.
		// Model tag and measurement fields below report verified state; they are not trust anchors.
		if enclave == nil {
			if client, err = em.connect(ctx, host, tinfoil.Pin{Repo: model.Repo, Project: model.Project}); err == nil {
				verified = client.current()
			}
		} else {
			verified, err = client.reverify(ctx)
		}
		if err != nil {
			return fmt.Errorf("failed to verify remote attestation: %w", err)
		}
	}

	model.mu.Lock()
	defer model.mu.Unlock()

	if _, configured := model.configuredHosts[host]; !configured {
		return fmt.Errorf("enclave %s is no longer configured", host)
	}

	model.Tag = verified.codeTag
	model.SourceMeasurement = verified.codeMeasurement

	if enclave == nil {
		cb := newCircuitBreaker()
		enclave = &Enclave{
			host:      host,
			modelName: modelName,
			client:    client,
			proxy:     newProxy(host, modelName, client, em.billingCollector, cb),
			metrics:   newEnclaveMetrics(host, modelName),
			cb:        cb,
			pricing:   em.ModelPricing,
		}
		model.installEnclaveLocked(host, enclave)
		enclave.updateOverloadConfig(model.overloadForHostLocked(host), model.spillPoliciesLocked(host))
		CircuitBreakerState.WithLabelValues(modelName, host).Set(float64(cbClosed))
	}
	return nil
}

// installEnclaveLocked puts e in the pool under host, retiring any enclave
// already registered there: the previous entry's metrics poller and breaker
// must be stopped, or they keep running with nothing referencing them. The
// caller holds m.mu.
func (m *Model) installEnclaveLocked(host string, e *Enclave) {
	if previous, ok := m.Enclaves[host]; ok && previous != e {
		previous.shutdown()
	}
	m.Enclaves[host] = e
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
		"observed_at": time.Now(),
		"models":      em.Models(),
		"errors":      errors,
		"updated":     updated,
		"attempted":   attempted,
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

// CacheRoutePool snapshots the model's replica set for cache-aware routing:
// breaker-closed enclaves with live in-flight counts, falling back to all
// enclaves when every breaker is open. The overload
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
		if !enclave.attestationValid(time.Now()) {
			continue
		}
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
			if !enclave.attestationValid(time.Now()) {
				continue
			}
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

// HasHealthyEnclave reports whether the caller's pools have an enclave with
// unexpired attestation and a closed circuit breaker. Used by the auto router
// to skip models whose backends cannot accept requests.
func (m *Model) HasHealthyEnclave(primary, spill map[string]bool) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, enclave := range m.Enclaves {
		if primary != nil && !primary[enclave.host] && !spill[enclave.host] {
			continue
		}
		if enclave.attestationValid(time.Now()) && (enclave.cb == nil || enclave.cb.Closed()) {
			return true
		}
	}
	return false
}

// breakerClosed reports whether the circuit breaker is closed (nil counts
// as closed, matching selection's treatment).
func (e *Enclave) breakerClosed() bool {
	return e.cb == nil || e.cb.Closed()
}

// ReleaseDispatch releases one SelectServing reservation after rejection,
// dispatch failure, or response completion. Each selection must release once.
func (e *Enclave) ReleaseDispatch() {
	e.inflight.Add(-1)
	BackendInflight.WithLabelValues(e.modelName, e.host).Dec()
}

func (e *Enclave) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tinfoil-Enclave", e.host)

	// Check if client requested usage metrics in response header/trailer
	usageMetricsRequested := r.Header.Get(UsageMetricsRequestHeader) == "true"
	if !usageMetricsRequested {
		e.proxy.ServeHTTP(w, r)
		return
	}

	// Wrap the ResponseWriter to capture usage and write trailer
	var pricing *ModelPricing
	if e.pricing != nil {
		if value, ok := e.pricing(e.modelName); ok {
			pricing = &value
		}
	}
	wrapper := &usageMetricsWriter{ResponseWriter: w, model: e.modelName, pricing: pricing}
	if pricing != nil && pricing.CostKnownWithoutUsage() {
		wrapper.usage = &tokencount.Usage{}
	}

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
	verified, _ := e.verified()
	fields := map[string]string{
		"predicate":  verified.predicate,
		"tls_key_fp": verified.tlsKeyFP,
	}
	if verified.hpkeKey != "" {
		fields["hpke_key"] = verified.hpkeKey
	}
	return json.Marshal(fields)
}

func (e *Enclave) String() string {
	return e.host
}

func validateControlPlaneURL(controlPlaneURL string, debug bool) error {
	if debug {
		return nil
	}
	parsed, err := url.Parse(controlPlaneURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("control plane URL must use HTTPS")
	}
	return nil
}

// NewEnclaveManager loads model repos and projects from the local config file (not remote) into the enclave manager
func NewEnclaveManager(configFile []byte, controlPlaneURL string, usageReporterID string, usageReporterSecret string, usageContextSecret string, initConfigURL string, updateConfigURL string, refreshInterval time.Duration, debug bool) (*EnclaveManager, error) {
	if refreshInterval <= 0 {
		return nil, fmt.Errorf("refresh interval must be positive, got %v", refreshInterval)
	}
	if err := validateControlPlaneURL(controlPlaneURL, debug); err != nil {
		return nil, err
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
	if err := cfg.ValidateTrustAnchors(); err != nil {
		return nil, err
	}

	em := &EnclaveManager{
		models:               &sync.Map{},
		initConfigURL:        initConfigURL,
		updateConfigURL:      updateConfigURL,
		controlPlaneURL:      controlPlaneURL,
		connect:              connectEnclave,
		billingCollector:     billing.NewCollector(controlPlaneURL, usageReporterID, usageReporterSecret),
		unknownModelReporter: billing.NewUnknownModelReporter(controlPlaneURL, usageReporterID, usageReporterSecret),
		usageContextSecret:   usageContextSecret,
		cacheRouteShadow:     cacheroute.NewShadow(nil),
		refreshInterval:      refreshInterval,
		debug:                debug,
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
		Project:           modelConfig.Project,
		Tag:               "",
		SourceMeasurement: nil,
		Enclaves:          make(map[string]*Enclave),
		Overload:          modelConfig.Overload,
		CacheRoute:        modelConfig.CacheRoute,
	}
	model.setConfiguredHosts(modelConfig.Hostnames)
	model.applyReservations(modelName, modelConfig.Reservations, modelConfig.Hostnames)
	em.models.Store(modelName, model)
	// Born at zero so rate() sees the first 503 after a restart.
	ModelUnavailableTotal.WithLabelValues(modelName).Add(0)
	cacheroute.SetMode(modelName, modelConfig.CacheRoute)
	cacheroute.SetPoolInfo(modelName, modelConfig.Hostnames)
}

func (m *Model) setConfiguredHosts(hostnames []string) {
	hosts := make(map[string]verificationAttempt, len(hostnames))
	for _, host := range hostnames {
		hosts[host] = m.configuredHosts[host]
	}
	m.configuredHosts = hosts
	m.expectedHosts = len(hostnames)
}

// sync applies available configuration and renews the accepted backend set.
func (em *EnclaveManager) sync() error {
	log.Debug("Updating all models")
	em.stateMu.Lock()
	em.lastAttemptedUpdate = time.Now()
	em.stateMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), runtimeConfigTimeout)
	defer cancel()
	config, configErr := config.LoadContext(ctx, em.updateConfigURL, false)
	if configErr == nil {
		em.refreshModelMetadata()
		em.models.Range(func(key, value any) bool {
			modelName := key.(string)
			model := value.(*Model)

			configModel, modelInConfig := config.Models[modelName]
			if !modelInConfig {
				log.Warnf("model %s no longer in config", modelName)
				model.mu.Lock()
				model.setConfiguredHosts(nil)
				model.applyReservations(modelName, nil, nil)
				for _, enclave := range model.Enclaves {
					enclave.shutdown()
				}
				model.Enclaves = make(map[string]*Enclave)
				model.mu.Unlock()
				cacheroute.DropModel(modelName)
				return true
			}

			// If the repo has changed, display a warning.
			if model.Repo != configModel.Repo {
				log.Errorf("repo changed for model %s: %s -> %s. Publish a new release and update this router if you want the change to take effect.", modelName, model.Repo, configModel.Repo)
				return true
			}
			log.Tracef("Updating config for model %s", modelName)
			model.mu.Lock()
			model.setConfiguredHosts(configModel.Hostnames)
			model.Overload = configModel.Overload
			model.CacheRoute = configModel.CacheRoute
			model.applyReservations(modelName, configModel.Reservations, configModel.Hostnames)
			for host, enclave := range model.Enclaves {
				enclave.updateOverloadConfig(model.overloadForHostLocked(host), model.spillPoliciesLocked(host))
			}
			cacheroute.SetMode(modelName, configModel.CacheRoute)
			cacheroute.SetPoolInfo(modelName, configModel.Hostnames)

			// Remove enclaves that are no longer in the config
			for existingHost := range model.Enclaves {
				if _, configured := model.configuredHosts[existingHost]; !configured {
					log.Warnf("hostname %s no longer in config, removing", existingHost)
					model.Enclaves[existingHost].shutdown()
					delete(model.Enclaves, existingHost)
				}
			}
			model.mu.Unlock()
			return true
		})
	}
	em.refreshEnclaves()
	if configErr != nil {
		return fmt.Errorf("failed to fetch config: %v", configErr)
	}

	em.stateMu.Lock()
	em.lastSuccessfulUpdate = time.Now()
	em.stateMu.Unlock()

	return nil
}

// Configuration availability must not prevent renewal of the accepted host set.
func (em *EnclaveManager) refreshEnclaves() {
	var wg sync.WaitGroup
	em.models.Range(func(key, value any) bool {
		model := value.(*Model)
		model.mu.RLock()
		hosts := maps.Clone(model.configuredHosts)
		model.mu.RUnlock()
		for host := range hosts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := em.addEnclave(key.(string), host); err != nil {
					em.stateMu.Lock()
					em.errors = append(em.errors, err.Error())
					em.stateMu.Unlock()
					log.Errorf("failed to refresh enclave %s: %v", host, err)
				}
			}()
		}
		return true
	})
	wg.Wait()
}

// StartWorker starts the worker update loop
func (em *EnclaveManager) StartWorker() {
	em.runWorker(context.Background())
}

func (em *EnclaveManager) runWorker(ctx context.Context) {
	ticker := time.NewTicker(min(em.refreshInterval, attestationRefreshInterval))
	defer ticker.Stop()

	for {
		em.stateMu.Lock()
		em.errors = []string{} // Clear errors
		em.stateMu.Unlock()
		if err := em.sync(); err != nil {
			log.Errorf("failed to update: %v", err)
			em.stateMu.Lock()
			em.errors = append(em.errors, err.Error())
			em.stateMu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Enclave) updateOverloadConfig(cfg *config.OverloadConfig, policies []*config.OverloadConfig) {
	if e == nil || e.metrics == nil {
		return
	}
	e.metrics.setPolicies(cfg, policies)
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
	BackendRunningRequests.DeleteLabelValues(e.modelName, e.host)
	BackendOverloaded.DeleteLabelValues(e.modelName, e.host)
	CircuitBreakerState.DeleteLabelValues(e.modelName, e.host)
}
