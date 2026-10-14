package manager

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-model-router/config"
	"github.com/tinfoilsh/tinfoil-go/verify"
	"github.com/tinfoilsh/tinfoil-go/verify/measurement"
)

const registryTestRef = "tinfoil/confidential-gpt-oss-120b"

func TestBackendTrustSeparatesAuthorities(t *testing.T) {
	for _, tc := range []struct {
		name, repo, configRef, verifiedRef string
		registry                           bool
		source                             verify.ReferenceSource
		accepted                           bool
	}{
		{name: "repository only", repo: renewalTestRepo, verifiedRef: renewalTestRepo, accepted: true},
		{name: "registry only", configRef: registryTestRef, verifiedRef: registryTestRef, registry: true, source: verify.SourceEndorsement, accepted: true},
		{name: "mixed repository", repo: renewalTestRepo, configRef: registryTestRef, verifiedRef: renewalTestRepo, accepted: true},
		{name: "mixed registry", repo: renewalTestRepo, configRef: registryTestRef, verifiedRef: registryTestRef, registry: true, source: verify.SourceEndorsement, accepted: true},
		{name: "registry cannot satisfy repo", repo: registryTestRef, verifiedRef: registryTestRef, registry: true, source: verify.SourceEndorsement},
		{name: "repository cannot satisfy config ref", configRef: renewalTestRepo, verifiedRef: renewalTestRepo},
		{name: "same name registry", repo: registryTestRef, configRef: registryTestRef, verifiedRef: registryTestRef, registry: true, source: verify.SourceEndorsement, accepted: true},
		{name: "same name repository", repo: registryTestRef, configRef: registryTestRef, verifiedRef: registryTestRef, accepted: true},
		{name: "embedded config is not an endorsement", configRef: registryTestRef, verifiedRef: registryTestRef, registry: true, source: verify.SourceEmbedded},
		{name: "missing anchors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const host = "backend.example"
			proof := renewalVerification("verified-key", time.Now().Add(time.Hour))
			if tc.registry {
				proof.Config = &verify.ConfigVerification{Source: tc.source, Name: "/" + tc.verifiedRef + "/v1"}
			}
			em := &EnclaveManager{verifier: func(target, reference string) (*verify.Verification, error) {
				if target != host || reference != tc.verifiedRef {
					return nil, errors.New("attestation does not match the expected identity")
				}
				return proof, nil
			}}
			verified, err := em.verifyBackend(host, &Model{Repo: tc.repo, ConfigRef: tc.configRef})
			if tc.accepted {
				if err != nil || verified != proof {
					t.Fatalf("authorized backend rejected: verification=%+v err=%v", verified, err)
				}
			} else if err == nil || verified != nil {
				t.Fatalf("unauthorized backend accepted: verification=%+v err=%v", verified, err)
			}
		})
	}
}

func TestMixedBackendPoolAndRegistryRenewal(t *testing.T) {
	classic, classicKey := renewalServer(t, nil)
	registry, registryKey := renewalServer(t, nil)
	classicHost := strings.TrimPrefix(classic.URL, "https://")
	registryHost := strings.TrimPrefix(registry.URL, "https://")
	cfg, err := config.FromBytes([]byte(fmt.Sprintf("models:\n  %s:\n    repo: %s\n    config_ref: %s\n    enclaves: [%q, %q]\n", renewalTestModel, renewalTestRepo, registryTestRef, classicHost, registryHost)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	classicProof := renewalVerification(classicKey, now.Add(time.Hour))
	registryProof := renewalVerification(registryKey, now.Add(time.Hour))
	registryProof.CodeTag = "v0.15.1"
	registryProof.CodeMeasurement = &measurement.Measurement{Type: measurement.SnpTdxMultiPlatformV1}
	registryProof.Config = &verify.ConfigVerification{Source: verify.SourceEndorsement, Name: "/" + registryTestRef + "/v1", Digest: strings.Repeat("a", sha256.Size*2)}
	em := &EnclaveManager{models: &sync.Map{}, refreshInterval: renewalTestRefresh, verifier: func(host, reference string) (*verify.Verification, error) {
		switch {
		case host == classicHost && reference == renewalTestRepo:
			return classicProof, nil
		case host == registryHost && reference == registryTestRef:
			return registryProof, nil
		default:
			return nil, errors.New("wrong workload identity")
		}
	}}
	em.addModel(renewalTestModel, cfg.Models[renewalTestModel])
	model, _ := em.GetModel(renewalTestModel)
	t.Cleanup(func() {
		for _, backend := range model.Enclaves {
			backend.shutdown()
		}
	})
	for _, host := range []string{classicHost, registryHost} {
		if err := em.addEnclave(renewalTestModel, host); err != nil {
			t.Fatal(err)
		}
		backend := model.Enclaves[host]
		transport := newAttestedTransport(backend.tlsKeyFP, backend.attestation)
		t.Cleanup(transport.CloseIdleConnections)
		requireRenewalResponse(t, transport, "https://"+host)
	}
	status, _ := readVerificationStatus(t, model)
	if status.Healthy != 2 || status.Hosts[classicHost].ConfigName != "" || status.Hosts[registryHost].ConfigName != registryProof.Config.Name || status.Hosts[registryHost].ConfigDigest != registryProof.Config.Digest || status.Hosts[registryHost].CodeTag != registryProof.CodeTag {
		t.Fatalf("mixed pool lost verified identity: %+v", status)
	}
	for host, proof := range map[string]*verify.Verification{classicHost: classicProof, registryHost: registryProof} {
		backend := status.Hosts[host]
		if backend.CodeTag != proof.CodeTag || backend.Measurement == nil || backend.Measurement.Type != proof.CodeMeasurement.Type {
			t.Fatalf("backend %s lost its release or measurement: %+v", host, backend)
		}
	}
	backend := model.Enclaves[registryHost]
	registryProof.Config.Name = "/" + registryTestRef + "/v2"
	registryProof.Config.Digest = strings.Repeat("b", sha256.Size*2)
	registryProof.FreshnessExpiresAt = now.Add(2 * time.Hour)
	if err := em.addEnclave(renewalTestModel, registryHost); err != nil {
		t.Fatal(err)
	}
	status, _ = readVerificationStatus(t, model)
	if model.Enclaves[registryHost] != backend || status.Hosts[registryHost].ConfigName != registryProof.Config.Name || status.Hosts[registryHost].ConfigDigest != registryProof.Config.Digest || !status.Hosts[registryHost].ExpiresAt.Equal(registryProof.FreshnessExpiresAt) {
		t.Fatalf("same-key renewal lost registry identity or serving state: %+v", status)
	}
	registryProof.FreshnessExpiresAt = now.Add(-time.Second)
	if err := em.addEnclave(renewalTestModel, registryHost); err == nil {
		t.Fatal("expired registry evidence replaced an accepted backend")
	}
	if !backend.attestation.load().expiresAt.Equal(now.Add(2 * time.Hour)) {
		t.Fatal("failed registry renewal changed the accepted expiration")
	}
}
