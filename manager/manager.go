package manager

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/verifier/attestation"
	"github.com/tinfoilsh/verifier/github"
	"github.com/tinfoilsh/verifier/sigstore"

	"github.com/tinfoilsh/confidential-model-router/billing"
	"github.com/tinfoilsh/confidential-model-router/config"
)

type Enclave struct {
	host      string
	tlsKeyFP  string
	hpkeKey   string
	predicate attestation.PredicateType
	proxy     *httputil.ReverseProxy
	metrics   *enclaveMetrics
}

type Model struct {
	Repo              string                   `json:"repo"`
	Tag               string                   `json:"tag"`
	SourceMeasurement *attestation.Measurement `json:"measurement"`
	Enclaves          map[string]*Enclave      `json:"enclaves"`
	Overload          *config.OverloadConfig   `json:"overload,omitempty"`

	counter uint64
	mu      sync.RWMutex
}

type EnclaveManager struct {
	models               *sync.Map // model name -> *Model
	initConfigURL        string
	updateConfigURL      string
	sigstoreClient       *sigstore.Client
	billingCollector     *billing.Collector
	refreshInterval      time.Duration
	errors               []string
	lastSuccessfulUpdate time.Time
	lastAttemptedUpdate  time.Time
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

	model.Enclaves[host] = &Enclave{
		host:      host,
		predicate: verification.Measurement.Type,
		tlsKeyFP:  verification.TLSPublicKeyFP,
		hpkeKey:   verification.HPKEPublicKey,
		proxy:     newProxy(host, verification.TLSPublicKeyFP, modelName, em.billingCollector),
		metrics:   newEnclaveMetrics(host, modelName),
	}
	model.Enclaves[host].updateOverloadConfig(model.Overload)
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

// Status returns the status of the enclave manager to be JSON encoded
func (em *EnclaveManager) Status() map[string]any {
	return map[string]any{
		"models":    em.Models(),
		"errors":    em.errors,
		"updated":   em.lastSuccessfulUpdate,
		"attempted": em.lastAttemptedUpdate,
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

// NextEnclave gets the next sequential enclave
func (m *Model) NextEnclave() *Enclave {
	if len(m.Enclaves) == 0 {
		return nil
	}

	count := atomic.AddUint64(&m.counter, 1)
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Convert map to slice for indexed access
	enclaves := make([]*Enclave, 0, len(m.Enclaves))
	for _, enclave := range m.Enclaves {
		enclaves = append(enclaves, enclave)
	}

	return enclaves[(count-1)%uint64(len(enclaves))]
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
	wrapper := &usageMetricsWriter{ResponseWriter: w}

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
func NewEnclaveManager(configFile []byte, controlPlaneURL string, initConfigURL string, updateConfigURL string, refreshInterval time.Duration) (*EnclaveManager, error) {
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
		models:           &sync.Map{},
		initConfigURL:    initConfigURL,
		updateConfigURL:  updateConfigURL,
		sigstoreClient:   sigstoreClient,
		billingCollector: billing.NewCollector(controlPlaneURL),
		refreshInterval:  refreshInterval,
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
	})
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
	em.lastAttemptedUpdate = time.Now()

	config, err := config.Load(em.updateConfigURL, false)
	if err != nil {
		return fmt.Errorf("failed to fetch config: %v", err)
	}

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
				for _, enclave := range model.Enclaves {
					enclave.shutdown()
				}
				model.mu.Unlock()
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
			model.Overload = configModel.Overload
			for _, enclave := range model.Enclaves {
				enclave.updateOverloadConfig(model.Overload)
			}

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
					em.errors = append(em.errors, err.Error())
					log.Errorf("failed to add enclave %s for model %s: %v", host, modelName, err)
				}
			}
		}()
		return true
	})
	wg.Wait()

	em.lastSuccessfulUpdate = time.Now()

	return nil
}

// StartWorker starts the worker update loop
func (em *EnclaveManager) StartWorker() {
	ticker := time.NewTicker(em.refreshInterval)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		em.errors = []string{} // Clear errors
		if err := em.sync(); err != nil {
			log.Errorf("failed to update: %v", err)
			em.errors = append(em.errors, err.Error())
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
