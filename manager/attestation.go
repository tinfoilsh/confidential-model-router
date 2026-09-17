package manager

import (
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	tinfoilClient "github.com/tinfoilsh/tinfoil-go/verifier/client"
	"github.com/tinfoilsh/tinfoil-go/verifier/envelope"
)

const (
	attestationConcurrency   = 4
	maxAttestationBytes      = 8 << 20
	attestationInterval      = 24 * time.Hour
	attestationRenewalWindow = time.Hour
)

// Bound network steps without changing the SDK's process-wide HTTP client.
var enclaveVerifyTimeout = 10 * time.Second

// Fetch failures do not invalidate a previously authenticated result, but
// they never extend its deadline. Invalid evidence does invalidate it.
type attestationFetchError struct{ error }

// A bounded, normal PKI-validated TLS handshake detects restarts without
// fetching or verifying a new CPU quote on every worker tick. This probe
// never authorizes a key: a changed key still requires full V3 verification.
func tlsPublicKeyFP(host string) (string, error) {
	u := url.URL{Host: host}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: enclaveVerifyTimeout}, "tcp",
		net.JoinHostPort(u.Hostname(), port), &tls.Config{})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return tinfoilClient.ConnectionCertFP(conn.ConnectionState())
}

func (em *EnclaveManager) refreshEnclave(modelName, host string) error {
	model, found := em.GetModel(modelName)
	if !found {
		return fmt.Errorf("model %s not found", modelName)
	}
	model.mu.RLock()
	current := model.Enclaves[host]
	due := current == nil || !current.attestationValid() || !time.Now().Before(current.nextAttestationAt)
	var expectedKey string
	if current != nil {
		expectedKey = current.tlsKeyFP
	}
	model.mu.RUnlock()
	if !due {
		probe := em.probeTLSKey
		if probe == nil {
			probe = tlsPublicKeyFP
		}
		key, err := probe(host)
		if err != nil {
			return fmt.Errorf("probing enclave %s TLS key: %w", host, err)
		}
		if key == expectedKey {
			return nil
		}
	}
	return em.addEnclave(modelName, host)
}

func verifyEnclaveV3(host, repo string) (*tinfoilClient.VerifiedDocumentV3, error) {
	nonce, err := envelope.RandomNonce()
	if err != nil {
		return nil, fmt.Errorf("generating attestation nonce: %w", err)
	}
	u := url.URL{Scheme: "https", Host: host, Path: "/.well-known/tinfoil-attestation",
		RawQuery: "nonce=" + hex.EncodeToString(nonce)}
	// Keep the attestation fetch bounded and hostname-isolated. Its certificate
	// is not the trust anchor: VerifyDocumentV3 pins repo and authenticates the
	// nonce, evidence, collateral and endorsed service keys. Service requests
	// below enforce the endorsed TLS key, including on reused connections.
	httpClient := &http.Client{
		Timeout:       enclaveVerifyTimeout,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer httpClient.CloseIdleConnections()
	resp, err := httpClient.Get(u.String())
	if err != nil {
		return nil, attestationFetchError{fmt.Errorf("fetching V3 attestation: %w", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, attestationFetchError{fmt.Errorf("fetching V3 attestation: HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttestationBytes+1))
	if err != nil {
		return nil, attestationFetchError{fmt.Errorf("reading V3 attestation: %w", err)}
	}
	if len(body) > maxAttestationBytes {
		return nil, fmt.Errorf("V3 attestation exceeds %d bytes", maxAttestationBytes)
	}
	// No latest-release lookup and no V2 fallback. Each endpoint must supply
	// the complete V3 evidence and fresh endorsements for its configured repo.
	return tinfoilClient.VerifyDocumentV3(body, nonce, repo)
}

func (em *EnclaveManager) addEnclave(modelName, host string) error {
	model, found := em.GetModel(modelName)
	if !found {
		return fmt.Errorf("model %s not found", modelName)
	}
	verify := em.verifyEnclave
	if verify == nil {
		verify = verifyEnclaveV3
	}
	// Repository identity is pinned by the initial configuration and immutable.
	// Never hold the model lock across attestation or other network I/O.
	verified, err := verify(host, model.Repo)
	var tlsKey, hpkeKey string
	if err == nil {
		if verified == nil || verified.EnclaveMeasurement == nil || !time.Now().Before(verified.FreshnessExpiresAt) {
			err = errors.New("V3 verification returned missing or expired evidence")
		} else if tlsKey, err = verified.TLSPublicKeyFP(); err == nil {
			hpkeKey, err = verified.HPKEPublicKey()
		}
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if err != nil {
		var fetchErr attestationFetchError
		if !errors.As(err, &fetchErr) {
			if previous := model.Enclaves[host]; previous != nil {
				previous.shutdown()
				delete(model.Enclaves, host)
			}
		}
		return fmt.Errorf("attesting %s: %w", host, err)
	}
	if !slices.Contains(model.hostnames, host) {
		return fmt.Errorf("enclave %s is no longer configured", host)
	}
	nextAttestation := time.Now().Add(attestationInterval)
	if renewAt := verified.FreshnessExpiresAt.Add(-attestationRenewalWindow); renewAt.Before(nextAttestation) {
		nextAttestation = renewAt
	}
	if previous := model.Enclaves[host]; previous != nil &&
		previous.tlsKeyFP == tlsKey && previous.hpkeKey == hpkeKey &&
		previous.predicate == verified.EnclaveMeasurement.Type {
		// Renewal must not reset load metrics, active streams or breaker state.
		previous.verification.Store(verified)
		previous.nextAttestationAt = nextAttestation
		return nil
	}
	cb := newCircuitBreaker()
	enclave := &Enclave{
		host: host, modelName: modelName,
		nextAttestationAt: nextAttestation,
		tlsKeyFP:          tlsKey, hpkeKey: hpkeKey, predicate: verified.EnclaveMeasurement.Type,
		proxy:   newProxy(host, tlsKey, modelName, em.billingCollector, cb),
		metrics: newEnclaveMetrics(host, modelName), cb: cb, pricing: em.ModelPricing,
	}
	enclave.verification.Store(verified)
	enclave.proxy.Transport = &attestationTransport{enclave: enclave, base: enclave.proxy.Transport}
	model.installEnclaveLocked(host, enclave)
	enclave.updateOverloadConfig(model.Overload)
	CircuitBreakerState.WithLabelValues(modelName, host).Set(float64(cbClosed))
	return nil
}

// refreshAttestations checks targets on the existing worker cadence (five
// minutes by default), even if fetching configuration fails. Healthy endpoints
// get a lightweight key probe; full verification is due daily, before proof
// expiry, or on key change. Missing endpoints and failed due verifications
// retry on each tick. Concurrency is bounded across the whole fleet.
func (em *EnclaveManager) refreshAttestations() {
	type target struct{ model, host string }
	jobs := make(chan target)
	var workers sync.WaitGroup
	for range attestationConcurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for target := range jobs {
				if err := em.refreshEnclave(target.model, target.host); err != nil {
					em.stateMu.Lock()
					em.errors = append(em.errors, err.Error())
					em.stateMu.Unlock()
					log.WithError(err).Warn("failed to refresh enclave attestation")
				}
			}
		}()
	}
	em.models.Range(func(key, value any) bool {
		model := value.(*Model)
		model.mu.RLock()
		hosts := slices.Clone(model.hostnames)
		model.mu.RUnlock()
		for _, host := range hosts {
			jobs <- target{key.(string), host}
		}
		return true
	})
	close(jobs)
	workers.Wait()
}

func (e *Enclave) attestationValid() bool {
	verified := e.verification.Load()
	return verified != nil && time.Now().Before(verified.FreshnessExpiresAt)
}

// Check on every new HTTP exchange, not just selection: MCP sessions may
// cache a client, and a selected endpoint may expire before dispatch. Existing
// streams are allowed to finish; no new exchange starts after expiry.
type attestationTransport struct {
	enclave *Enclave
	base    http.RoundTripper
}

func (t *attestationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.enclave.attestationValid() {
		return nil, errors.New("enclave attestation is unavailable or expired")
	}
	return t.base.RoundTrip(req)
}

func (e *Enclave) attestedTransport() http.RoundTripper {
	return &attestationTransport{enclave: e, base: &tinfoilClient.TLSBoundRoundTripper{ExpectedPublicKey: e.tlsKeyFP}}
}
