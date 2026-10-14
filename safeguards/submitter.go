package safeguards

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	log "github.com/sirupsen/logrus"
)

// ConversationIDHeader carries the client's stable chat id. It is consumed by
// the router and never forwarded upstream.
const ConversationIDHeader = "X-Tinfoil-Conversation-Id"

const (
	ingestPath          = "/ingest"
	requestTimeout      = 5 * time.Second
	drainTimeout        = 10 * time.Second
	maxSubmissionBytes  = 1 << 20
	messageJSONOverhead = len(`{"role":"","content":""},`)
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
	pending  chan []byte
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
		pending:  make(chan []byte, maxPending),
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
	body, ok := encodeSubmission(credential, conversationID, messages)
	if !ok {
		submissionsTotal.WithLabelValues("oversized").Inc()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.pending <- body:
	default:
		submissionsTotal.WithLabelValues("dropped").Inc()
	}
}

// encodeSubmission serializes the conversation within maxSubmissionBytes,
// trimming the oldest history when all of it does not fit. It fails only when
// not even part of the latest turn fits.
func encodeSubmission(credential, conversationID string, messages []Message) ([]byte, bool) {
	if fitsSubmission(credential, conversationID, messages) {
		body, err := json.Marshal(submission{Credential: credential, ConversationID: conversationID, Messages: messages})
		if err == nil && len(body) <= maxSubmissionBytes {
			return body, true
		}
	}
	messages = trimToFit(credential, conversationID, messages)
	if len(messages) == 0 {
		return nil, false
	}
	body, err := json.Marshal(submission{Credential: credential, ConversationID: conversationID, Messages: messages})
	if err != nil || len(body) > maxSubmissionBytes {
		return nil, false
	}
	return body, true
}

// trimToFit returns the newest messages whose encoding fits in a submission.
// The oldest turns are dropped first and the oldest turn kept is truncated
// from the front to fit the remaining space, so the latest turns always
// reach the classifier. It returns nil when nothing fits.
func trimToFit(credential, conversationID string, messages []Message) []Message {
	envelope, err := json.Marshal(submission{Credential: credential, ConversationID: conversationID, Messages: []Message{}})
	if err != nil {
		return nil
	}
	remaining := maxSubmissionBytes - len(envelope)
	var kept []Message
	for i := len(messages) - 1; i >= 0; i-- {
		if len(kept) > 0 {
			remaining -= len(",")
		}
		m, size, ok := fitMessage(messages[i], remaining)
		if !ok {
			break
		}
		kept = append(kept, m)
		remaining -= size
		if len(m.Content) < len(messages[i].Content) {
			break
		}
	}
	slices.Reverse(kept)
	return kept
}

// fitMessage returns m, or m with its content truncated from the front, whose
// encoding fits in budget bytes, along with that encoded size. ok is false
// when none of the content fits.
func fitMessage(m Message, budget int) (Message, int, bool) {
	if len(m.Role)+len(m.Content)+messageJSONOverhead <= budget {
		if b, err := json.Marshal(m); err == nil && len(b) <= budget {
			return m, len(b), true
		}
	}
	empty, err := json.Marshal(Message{Role: m.Role})
	if err != nil {
		return Message{}, 0, false
	}
	// JSON encodes a content byte as at most 6 bytes (\u00XX, \ufffd), so a
	// tail cut to a sixth of the room fits however it escapes. Cloned so the
	// kept tail does not pin the full original content.
	cut := Message{Role: m.Role, Content: strings.Clone(tail(m.Content, (budget-len(empty))/6))}
	b, err := json.Marshal(cut)
	if err != nil || len(b) > budget || cut.Content == "" {
		return Message{}, 0, false
	}
	return cut, len(b), true
}

// tail returns at most n trailing bytes of s without splitting a UTF-8 sequence.
func tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// fitsSubmission bounds the input before JSON encoding; the encoded length is
// checked separately because escaping can make it larger.
func fitsSubmission(credential, conversationID string, messages []Message) bool {
	remaining := maxSubmissionBytes
	for _, value := range []string{credential, conversationID} {
		if len(value) > remaining {
			return false
		}
		remaining -= len(value)
	}
	for _, message := range messages {
		if messageJSONOverhead > remaining {
			return false
		}
		remaining -= messageJSONOverhead
		for _, value := range []string{message.Role, message.Content} {
			if len(value) > remaining {
				return false
			}
			remaining -= len(value)
		}
	}
	return true
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

func (s *Submitter) deliver(ctx context.Context, body []byte) {
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
