package manager

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/verifier/attestation"
	"github.com/tinfoilsh/verifier/github"
	"github.com/tinfoilsh/verifier/sigstore"

	"github.com/tinfoilsh/confidential-inference-proxy/billing"
	"github.com/tinfoilsh/confidential-inference-proxy/config"
)

type Enclave struct {
	host      string
	tlsKeyFP  string
	hpkeKey   string
	predicate attestation.PredicateType
	proxy     *httputil.ReverseProxy
}

type Model struct {
	Repo              string                   `json:"repo"`
	Tag               string                   `json:"tag"`
	SourceMeasurement *attestation.Measurement `json:"measurement"`
	Enclaves          map[string]*Enclave      `json:"enclaves"`

	counter uint64
	mu      sync.RWMutex
}

type EnclaveManager struct {
	models               *sync.Map // model name -> *Model
	configURL            string
	sigstoreClient       *sigstore.Client
	billingCollector     *billing.Collector
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
		realTLSKeyFP, err := attestation.TLSPublicKey(host)
		if err == nil && currentEnclave.tlsKeyFP == realTLSKeyFP {
			log.Debugf("enclave %s already exists and TLS key fingerprint is the same, skipping", host)
			return nil
		}
	}

	remoteAttestation, err := attestation.Fetch(host)
	if err != nil {
		return fmt.Errorf("failed to fetch remote attestation: %v", err)
	}
	verification, err := remoteAttestation.Verify()
	if err != nil {
		return fmt.Errorf("failed to verify remote attestation: %v", err)
	}
	if verification.Measurement.Type == attestation.TdxGuestV1 {
		_, err = attestation.VerifyHardware(hwMeasurements, verification.Measurement)
		if err != nil {
			return fmt.Errorf("failed to verify hardware measurements: %v", err)
		}
	}

	// Validate that the enclave's attested measurement matches the model's source measurement
	if model.SourceMeasurement != nil {
		if err := verification.Measurement.Equals(model.SourceMeasurement); err != nil {
			return fmt.Errorf("measurement mismatch for enclave %s: %v", host, err)
		}
	}

	model.Enclaves[host] = &Enclave{
		host:      host,
		predicate: verification.Measurement.Type,
		tlsKeyFP:  verification.TLSPublicKeyFP,
		hpkeKey:   verification.HPKEPublicKey,
		proxy:     newProxy(host, verification.TLSPublicKeyFP, modelName, em.billingCollector),
	}
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

// Shutdown gracefully stops the billing collector
func (em *EnclaveManager) Shutdown() {
	if em.billingCollector != nil {
		em.billingCollector.Stop()
	}
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
	e.proxy.ServeHTTP(w, r)
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

// NewEnclaveManager loads model repos from the config into the enclave manager
func NewEnclaveManager(configFile []byte, controlPlaneURL string, configURL string) (*EnclaveManager, error) {
	cfg, err := config.FromBytes(configFile)
	if err != nil {
		return nil, err
	}

	sigstoreClient, err := sigstore.NewClient()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch trust root: %v", err)
	}

	em := &EnclaveManager{
		models:           &sync.Map{},
		configURL:        configURL,
		sigstoreClient:   sigstoreClient,
		billingCollector: billing.NewCollector(controlPlaneURL),
	}

	for modelName, modelConfig := range cfg.Models {
		em.models.Store(modelName, &Model{
			Repo:              modelConfig.Repo,
			Tag:               "",
			SourceMeasurement: nil,
			Enclaves:          make(map[string]*Enclave),
		})
	}

	log.Infof("Loaded %d model(s) from initial config", len(cfg.Models))

	return em, nil
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
	measurement, err := em.sigstoreClient.VerifyAttestation(sigstoreBundle, digest, model.Repo)
	if err != nil {
		return false, fmt.Errorf("failed to verify attestation: %v", err)
	}

	model.mu.Lock()
	defer model.mu.Unlock()

	model.Tag = latestTag
	model.SourceMeasurement = measurement
	model.Enclaves = make(map[string]*Enclave) // Clear all enclaves, their measurements are now invalid

	return true, nil
}

// sync updates all model's tags and measurements, then matches them to the enclave config
func (em *EnclaveManager) sync() error {
	log.Debug("Updating all models")
	em.lastAttemptedUpdate = time.Now()

	config, err := config.Load(em.configURL)
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
			_, err := em.updateModelMeasurements(modelName)
			if err != nil {
				log.Errorf("failed to update model measurements: %v", err)
			}

			log.Tracef("Updating enclave config for model %s", modelName)
			enclaves := config.Models[modelName].Enclaves
			for _, enclave := range enclaves {
				err := func() error {
					// Remove enclaves that are no longer in the config
					model, found := em.GetModel(modelName)
					if !found {
						return fmt.Errorf("model %s not found", modelName)
					}
					for existingHost := range model.Enclaves {
						if !slices.Contains(enclaves, existingHost) {
							log.Warnf("Enclave %s no longer in config, removing", existingHost)
							delete(model.Enclaves, existingHost)
						}
					}

					log.Tracef("  + enclave %s", enclave)
					if err := em.addEnclave(modelName, enclave, hwMeasurements); err != nil {
						return fmt.Errorf("failed to add enclave: %v", err)
					}
					return nil
				}()
				if err != nil {
					em.errors = append(em.errors, err.Error())
					log.Warn(err)
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
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		em.errors = []string{} // Clear errors
		if err := em.sync(); err != nil {
			log.Errorf("failed to update: %v", err)
			em.errors = append(em.errors, err.Error())
		}
	}
}
