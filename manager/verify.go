package manager

import (
	"fmt"

	"github.com/tinfoilsh/verifier/attestation"
	"github.com/tinfoilsh/verifier/github"
	"github.com/tinfoilsh/verifier/sigstore"
)

func verifyRepo(repo, optionalTag string) (*attestation.Measurement, string, error) {
	var tag string
	var err error
	if optionalTag != "" {
		tag = optionalTag
	} else {
		tag, err = github.FetchLatestTag(repo)
	}

	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch latest tag: %v", err)
	}
	digest, err := github.FetchDigest(repo, tag)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch latest release: %v", err)
	}

	sigstoreBundle, err := github.FetchAttestationBundle(repo, digest)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch attestation bundle: %v", err)
	}

	trustRootJSON, err := sigstore.FetchTrustRoot()
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch trust root: %v", err)
	}

	measurement, err := sigstore.VerifyAttestation(trustRootJSON, sigstoreBundle, digest, repo)
	if err != nil {
		return nil, "", fmt.Errorf("failed to verify attestation: %v", err)
	}
	return measurement, tag, nil
}

func verifyEnclave(host string) (*attestation.Verification, error) {
	remoteAttestation, err := attestation.Fetch(host)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch remote attestation: %v", err)
	}

	verification, err := remoteAttestation.Verify()
	if err != nil {
		return nil, fmt.Errorf("failed to verify remote attestation: %v", err)
	}

	return verification, nil
}
