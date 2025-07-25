package manager

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/verifier/attestation"
	tinfoilClient "github.com/tinfoilsh/verifier/client"
	"gopkg.in/yaml.v2"

	"github.com/tinfoilsh/confidential-inference-proxy/billing"
	"github.com/tinfoilsh/confidential-inference-proxy/tokencount"
)

type Enclave struct {
	host        string
	publicKeyFP string
	proxy       *httputil.ReverseProxy
}

type Model struct {
	Repo              string                   `json:"repo"`
	Tag               string                   `json:"tag"`
	SourceMeasurement *attestation.Measurement `json:"measurement"`
	Enclaves          []*Enclave               `json:"enclaves"`

	counter uint64
	mu      sync.RWMutex
}

type EnclaveManager struct {
	models               *sync.Map // model name -> *Model
	hardwareMeasurements []*attestation.HardwareMeasurement
	billingCollector     *billing.Collector
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

func newProxy(host, publicKeyFP, modelName string, billingCollector *billing.Collector) *httputil.ReverseProxy {
	httpClient := &http.Client{
		Transport: &tinfoilClient.TLSBoundRoundTripper{
			ExpectedPublicKey: publicKeyFP,
		},
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "https",
		Host:   host,
	})
	proxy.Transport = httpClient.Transport

	// Add token extraction and billing via ModifyResponse
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Extract request details that we'll need for billing
		req := resp.Request
		authHeader := req.Header.Get("Authorization")
		userID := ""
		if authHeader != "" {
			// Use a placeholder for debugging without leaking credentials
			userID = "authenticated_user"
		}

		requestID := resp.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = resp.Header.Get("X-Request-ID")
		}

		requestPath := req.URL.Path
		streaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

		// Create a usage handler that will be called for streaming responses
		usageHandler := func(usage *tokencount.Usage) {
			if usage != nil && billingCollector != nil {
				event := billing.Event{
					Timestamp:        time.Now(),
					UserID:           userID,
					Model:            modelName,
					PromptTokens:     usage.PromptTokens,
					CompletionTokens: usage.CompletionTokens,
					TotalTokens:      usage.TotalTokens,
					RequestID:        requestID,
					Enclave:          host,
					RequestPath:      requestPath,
					Streaming:        streaming,
				}
				billingCollector.AddEvent(event)
			}
		}

		// Extract tokens with the usage handler
		newBody, _, err := tokencount.ExtractTokensFromResponseWithHandler(resp, modelName, usageHandler)
		if err != nil {
			log.WithError(err).Error("Failed to extract tokens from response")
			// Don't fail the request, just log the error
			return nil
		}

		// Replace the response body with our new reader
		resp.Body = newBody

		return nil
	}

	return proxy
}

// AddEnclave adds an enclave to the model enclave pool
func (em *EnclaveManager) AddEnclave(modelName, host string) error {
	model, found := em.GetModel(modelName)
	if !found {
		return fmt.Errorf("model %s not found", modelName)
	}

	// Check if the enclave already exists
	for _, existingEnclave := range model.Enclaves {
		if existingEnclave.host == host {
			return fmt.Errorf("enclave %s already exists", host)
		}
	}

	verification, err := verifyEnclave(host, em.hardwareMeasurements)
	if err != nil {
		return fmt.Errorf("failed to verify enclave %s: %w", host, err)
	}

	if err := verification.Measurement.Equals(model.SourceMeasurement); err != nil {
		return fmt.Errorf("measurement mismatch: %w", err)
	}

	model.mu.Lock()
	defer model.mu.Unlock()

	model.Enclaves = append(model.Enclaves, &Enclave{
		host:        host,
		publicKeyFP: verification.PublicKeyFP,
		proxy:       newProxy(host, verification.PublicKeyFP, modelName, em.billingCollector),
	})
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

// UpdateModel update's a model's tag and measurement, and all enclave's measurements
func (em *EnclaveManager) UpdateModel(modelName string) error {
	model, found := em.GetModel(modelName)
	if !found {
		return fmt.Errorf("model %s not found", modelName)
	}

	model.mu.Lock()
	defer model.mu.Unlock()

	measurement, tag, hwMeasurements, err := verifyRepo(model.Repo, "")
	if err != nil {
		return fmt.Errorf("failed to verify repo %s: %w", model.Repo, err)
	}
	model.Tag = tag
	model.SourceMeasurement = measurement
	em.hardwareMeasurements = hwMeasurements

	for _, enclave := range model.Enclaves {
		verification, err := verifyEnclave(enclave.host, em.hardwareMeasurements)
		if err != nil {
			return fmt.Errorf("failed to verify enclave %s: %w", enclave.host, err)
		}
		enclave.publicKeyFP = verification.PublicKeyFP
		enclave.proxy = newProxy(enclave.host, verification.PublicKeyFP, modelName, em.billingCollector)
	}

	return nil
}

// NextEnclave gets the next sequential enclave
func (m *Model) NextEnclave() *Enclave {
	if len(m.Enclaves) == 0 {
		return nil
	}

	count := atomic.AddUint64(&m.counter, 1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Enclaves[(count-1)%uint64(len(m.Enclaves))]
}

func (e *Enclave) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tinfoil-Enclave", e.host)
	e.proxy.ServeHTTP(w, r)
}

func (e *Enclave) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{
		"host": e.host,
		"key":  e.publicKeyFP,
	})
}

func (e *Enclave) UnmarshalJSON(data []byte) error {
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	e.host = m["host"]
	e.publicKeyFP = m["key"]
	return nil
}

func (e *Enclave) String() string {
	return e.host
}

// DeleteEnclave removes an enclave from the model enclave pool
func (em *EnclaveManager) DeleteEnclave(modelName, host string) error {
	model, found := em.GetModel(modelName)
	if !found {
		return fmt.Errorf("model %s not found", modelName)
	}

	model.mu.Lock()
	defer model.mu.Unlock()

	for i, enclave := range model.Enclaves {
		if enclave.host == host {
			// Remove the enclave from the slice
			model.Enclaves = append(model.Enclaves[:i], model.Enclaves[i+1:]...)
			return nil
		}
	}

	return fmt.Errorf("enclave %s not found", host)
}

// NewEnclaveManager loads model repos from the config, verifies them, and returns a map of verified models
func NewEnclaveManager(configFile []byte) (*EnclaveManager, error) {
	var config struct {
		Models map[string]string `json:"models"` // model name -> repo
	}
	if err := yaml.Unmarshal(configFile, &config); err != nil {
		return nil, err
	}

	models := make(map[string]*Model)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, 1)

	em := &EnclaveManager{
		models: &sync.Map{},
	}

	for modelName, repo := range config.Models {
		wg.Add(1)
		go func(modelName, repo string) {
			defer wg.Done()
			log.Debugf("verifying model %s (%s)", modelName, repo)

			tag := ""
			if strings.Contains(repo, "@") {
				parts := strings.Split(repo, "@")
				repo = parts[0]
				tag = parts[1]
			}

			measurement, tag, hwMeasurements, err := verifyRepo(repo, tag)
			if err != nil {
				select {
				case errCh <- fmt.Errorf("failed to verify repo %s: %w", repo, err):
				default:
				}
				return
			}

			mu.Lock()
			em.hardwareMeasurements = hwMeasurements
			models[modelName] = &Model{
				Repo:              repo,
				Tag:               tag,
				SourceMeasurement: measurement,
				Enclaves:          make([]*Enclave, 0),
			}
			mu.Unlock()
		}(modelName, repo)
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()
	if err := <-errCh; err != nil {
		return nil, err
	}

	log.Infof("Verified %d models", len(models))
	for k, v := range models {
		em.models.Store(k, v)
	}

	em.billingCollector = billing.NewCollector()
	return em, nil
}
