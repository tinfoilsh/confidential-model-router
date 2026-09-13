package safeguards

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	log "github.com/sirupsen/logrus"
)

// ConversationIDHeader carries the client's stable chat id. It is consumed by
// the router and never forwarded upstream.
const ConversationIDHeader = "X-Tinfoil-Conversation-Id"

const (
	ingestPath     = "/ingest"
	requestTimeout = 5 * time.Second
	drainTimeout   = 10 * time.Second
	// maxPending bounds conversations waiting for a delivery slot; beyond it
	// new completions are dropped rather than queued without limit.
	maxPending = 256
	// maxInFlight bounds concurrent requests to the sidecar.
	maxInFlight = 8
	// maxConversationIDLen keeps an attacker-controlled header from carrying
	// arbitrary payload into the sidecar or control plane.
	maxConversationIDLen = 128
)

var (
	submissionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "safeguards_submissions_total",
		Help: "Conversations handed to the safeguards sidecar, by outcome.",
	}, []string{"outcome"})
)

type submission struct {
	Credential     string    `json:"credential"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Messages       []Message `json:"messages"`
}

// Submitter delivers completed conversations to the sidecar without ever
// blocking or failing the inference request that produced them.
type Submitter struct {
	endpoint string
	client   *http.Client
	pending  chan submission
	wg       sync.WaitGroup
	cancel   context.CancelFunc

	mu     sync.Mutex
	closed bool
}

// NewSubmitter returns nil when the sidecar is not configured; a nil
// Submitter is safe to call and does nothing.
func NewSubmitter(baseURL string) *Submitter {
	if baseURL == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Submitter{
		endpoint: strings.TrimRight(baseURL, "/") + ingestPath,
		client:   &http.Client{Timeout: requestTimeout},
		pending:  make(chan submission, maxPending),
		cancel:   cancel,
	}
	for i := 0; i < maxInFlight; i++ {
		s.wg.Add(1)
		go s.worker(ctx)
	}
	return s
}

// Submit enqueues a completed conversation. Conversations with no assistant
// content are skipped: there is nothing for the policy to judge.
func (s *Submitter) Submit(credential, conversationID string, messages []Message) {
	if s == nil || len(messages) == 0 || messages[len(messages)-1].Role != "assistant" || messages[len(messages)-1].Content == "" {
		return
	}
	if len(conversationID) > maxConversationIDLen {
		conversationID = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.pending <- submission{Credential: credential, ConversationID: conversationID, Messages: messages}:
	default:
		submissionsTotal.WithLabelValues("dropped").Inc()
	}
}

// Close stops accepting work, drains what is already queued, and cancels
// anything still in flight after drainTimeout.
func (s *Submitter) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.pending)
	}
	s.mu.Unlock()

	timer := time.AfterFunc(drainTimeout, s.cancel)
	s.wg.Wait()
	timer.Stop()
	s.cancel()
}

func (s *Submitter) worker(ctx context.Context) {
	defer s.wg.Done()
	for sub := range s.pending {
		if ctx.Err() != nil {
			return
		}
		s.deliver(ctx, sub)
	}
}

func (s *Submitter) deliver(ctx context.Context, sub submission) {
	body, err := json.Marshal(sub)
	if err != nil {
		submissionsTotal.WithLabelValues("error").Inc()
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		submissionsTotal.WithLabelValues("error").Inc()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		submissionsTotal.WithLabelValues("error").Inc()
		log.WithError(err).Warn("safeguards submission failed")
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		submissionsTotal.WithLabelValues("rejected").Inc()
		log.WithField("status", resp.StatusCode).Warn("safeguards sidecar rejected submission")
		return
	}
	submissionsTotal.WithLabelValues("accepted").Inc()
}
