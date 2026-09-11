package billing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

// UnknownModelPath is the controlplane endpoint that receives rejected
// requests for model names the router does not serve.
const UnknownModelPath = "/api/internal/unknown-model-requests"

const (
	unknownModelReportTimeout = 5 * time.Second
	// unknownModelMaxInflight caps concurrent reports so a flood of unknown
	// model requests cannot pile up goroutines and connections; excess
	// reports are dropped, since the controlplane only needs one per owner.
	unknownModelMaxInflight = 16
)

// UnknownModelReport is the payload sent to the controlplane.
type UnknownModelReport struct {
	APIKey string `json:"api_key"`
	Model  string `json:"model"`
}

// UnknownModelReporter tells the controlplane when an authenticated request
// targets a model the router does not serve, so the owner can be told the
// model is gone. The controlplane dedupes per owner and model.
type UnknownModelReporter struct {
	endpoint   string
	reporterID string
	secret     string
	client     *http.Client
	inflight   chan struct{}
}

// NewUnknownModelReporter returns nil when reporting is not configured.
func NewUnknownModelReporter(controlPlaneURL, reporterID, secret string) *UnknownModelReporter {
	if controlPlaneURL == "" || secret == "" {
		return nil
	}
	return &UnknownModelReporter{
		endpoint:   strings.TrimRight(controlPlaneURL, "/") + UnknownModelPath,
		reporterID: reporterID,
		secret:     secret,
		client:     &http.Client{Timeout: unknownModelReportTimeout},
		inflight:   make(chan struct{}, unknownModelMaxInflight),
	}
}

// Report sends the rejection asynchronously; the caller has already
// answered the client and must not wait on the controlplane.
func (r *UnknownModelReporter) Report(apiKey, model string) {
	if r == nil || apiKey == "" || model == "" {
		return
	}
	select {
	case r.inflight <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-r.inflight }()
		r.send(UnknownModelReport{APIKey: apiKey, Model: model})
	}()
}

func (r *UnknownModelReporter) send(report UnknownModelReport) {
	body, err := json.Marshal(report)
	if err != nil {
		log.WithError(err).Error("failed to marshal unknown model report")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), unknownModelReportTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		log.WithError(err).Error("failed to build unknown model report")
		return
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		log.WithError(err).Error("failed to generate unknown model report nonce")
		return
	}
	nonce := hex.EncodeToString(nonceBytes)
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(usagereporting.HeaderReporterID, r.reporterID)
	req.Header.Set(usagereporting.HeaderTimestamp, timestamp)
	req.Header.Set(usagereporting.HeaderNonce, nonce)
	req.Header.Set(usagereporting.HeaderSignature,
		usagereporting.SignBatch(http.MethodPost, req.URL.Path, r.reporterID, timestamp, nonce, body, r.secret))

	resp, err := r.client.Do(req)
	if err != nil {
		log.WithError(err).WithField("model", report.Model).Warn("unknown model report failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.WithFields(log.Fields{"model": report.Model, "status": resp.StatusCode}).Warn("unknown model report rejected")
	}
}
