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

	// inflight counts requests currently being proxied through this
	// enclave. Unlike the polled queue depth (up to ~45s stale), it is
	// live, which is what least-loaded selection within a hot key's warm
	// set needs (design doc §5.5).
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

	expectedHosts int // number of configured hostnames; 0 means no backends expected
	mu            sync.RWMutex
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

// NextEnclave picks a uniformly random enclave with a closed circuit breaker,
// preferring those that are not currently overloaded and not in skip. If an
// open breaker is past its cooldown, it sends a probe to that enclave. Falls
// back to any closed enclave, then any enclave (degraded > unavailable).
func (m *Model) NextEnclave(skip map[string]bool) *Enclave {
	m.mu.RLock()
	defer m.mu.RUnlock()

	all := make([]*Enclave, 0, len(m.Enclaves))
	var closed, preferred []*Enclave
	for _, enclave := range m.Enclaves {
		all = append(all, enclave)
		if enclave.cb != nil && !enclave.cb.Closed() {
			if enclave.cb.NeedProbe() {
				return enclave
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
		return preferred[rand.IntN(len(preferred))]
	case len(closed) > 0:
		return closed[rand.IntN(len(closed))]
	case len(all) > 0:
		return all[rand.IntN(len(all))]
	}
	return nil
}

// EnclaveCount returns the number of configured enclaves under the model lock.
// Callers that need to bound a retry loop on enclave cardinality should use
// this rather than reading len(m.Enclaves) directly.
func (m *Model) EnclaveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Enclaves)
}

// CacheRoutePool snapshots the model's replica set for the cache-route
// shadow: the breaker-closed enclaves with their live in-flight counts —
// the stable HRW membership of the design doc's §5.3, deliberately ignoring
// the overload flag so a flapping load signal can't re-home keys — falling
// back to all enclaves when every breaker is open, mirroring NextEnclave's
// degraded fallback.
func (m *Model) CacheRoutePool() cacheroute.Pool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	pool := cacheroute.Pool{Size: len(m.Enclaves)}
	for _, enclave := range m.Enclaves {
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
		refreshInterval:    refreshInterval,
	}

	for modelName, modelConfig := range cfg.Models {
		em.addModel(modelName, modelConfig)
	}

	log.Infof("Loaded %d model(s) from initial config", len(cfg.Models))

	return em, nil
}

func (em *EnclaveManager) addModel(modelName string, modelConfig config.Model) {
	em.models.Store(modelName, &Model{
		Repo:              modelConfig.Repo,
		Tag:               "",
		SourceMeasurement: nil,
		Enclaves:          make(map[string]*Enclave),
		Overload:          modelConfig.Overload,
		RateLimit:         modelConfig.RateLimit,
		CacheRoute:        modelConfig.CacheRoute,
		expectedHosts:     len(modelConfig.Hostnames),
	})
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

func (e *Enclave) shutdown() {
	if e == nil || e.metrics == nil {
		return
	}
	e.metrics.shutdown()
}

func (e *Enclave) ShouldReject() (bool, time.Duration, float64) {
	if e == nil || e.metrics == nil {
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
