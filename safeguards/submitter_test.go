package safeguards

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSubmitter_DeliversCompletedConversation(t *testing.T) {
	var mu sync.Mutex
	var got []submission
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sub submission
		if r.URL.Path != ingestPath || json.NewDecoder(r.Body).Decode(&sub) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, sub)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer sidecar.Close()

	s := NewSubmitter(sidecar.URL + "/")
	s.Submit("tok", "chat-1", []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}})
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Credential != "tok" || got[0].ConversationID != "chat-1" || len(got[0].Messages) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestSubmitter_TrimsOversizedHistory(t *testing.T) {
	big := strings.Repeat("x", maxSubmissionBytes)
	for _, tc := range []struct {
		name     string
		messages []Message
	}{
		{"history", []Message{{Role: "user", Content: big + "the question"}, {Role: "assistant", Content: "reply"}}},
		{"older turns", []Message{{Role: "user", Content: big}, {Role: "assistant", Content: big}, {Role: "user", Content: "latest"}, {Role: "assistant", Content: "reply"}}},
		{"escaping", []Message{{Role: "assistant", Content: strings.Repeat("<\x00", maxSubmissionBytes/2)}}},
		{"multibyte", []Message{{Role: "user", Content: strings.Repeat("é", maxSubmissionBytes)}, {Role: "assistant", Content: "reply"}}},
		{"many messages", append(make([]Message, maxSubmissionBytes/messageJSONOverhead), Message{Role: "assistant", Content: "reply"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Submitter{pending: make(chan []byte, maxPending)}
			s.Submit("token", "chat", tc.messages)
			if len(s.pending) != 1 {
				t.Fatal("oversized history must be trimmed, not dropped")
			}
			body := <-s.pending
			if len(body) > maxSubmissionBytes || len(body) <= maxSubmissionBytes-messageJSONOverhead {
				t.Fatalf("body is %d bytes, want the trimmed history to fill up to %d", len(body), maxSubmissionBytes)
			}
			var got submission
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			offset := len(tc.messages) - len(got.Messages)
			for i, m := range got.Messages {
				want := tc.messages[offset+i]
				if m.Role != want.Role {
					t.Fatalf("message %d role = %q, want %q", i, m.Role, want.Role)
				}
				if i == 0 {
					if !utf8.ValidString(m.Content) || !strings.HasSuffix(want.Content, m.Content) {
						t.Fatal("the oldest kept message must be a valid trailing part of the original")
					}
					continue
				}
				if m.Content != want.Content {
					t.Fatalf("message %d must be kept whole", i)
				}
			}
		})
	}
}

func TestSubmitter_RejectsOversizedCredential(t *testing.T) {
	s := &Submitter{pending: make(chan []byte, maxPending)}
	s.Submit(strings.Repeat("x", maxSubmissionBytes), "chat", []Message{{Role: "assistant", Content: "reply"}})
	if len(s.pending) != 0 {
		t.Fatal("a submission whose envelope alone is oversized must not enter the queue")
	}
}

func TestSubmitter_QueuedPayloadOwnsItsData(t *testing.T) {
	s := &Submitter{pending: make(chan []byte, maxPending)}
	messages := []Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "reply"}}
	s.Submit("token", "chat", messages)
	messages[0].Content = strings.Repeat("x", maxSubmissionBytes)
	body := <-s.pending
	var got submission
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(body) > maxSubmissionBytes || got.Messages[0].Content != "hello" {
		t.Fatal("queue must own bounded serialized data rather than retain the caller's slice")
	}
}

func TestSubmitter_SkipsConversationsWithoutAReply(t *testing.T) {
	calls := 0
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer sidecar.Close()

	s := NewSubmitter(sidecar.URL)
	s.Submit("tok", "", nil)
	s.Submit("tok", "", []Message{{Role: "user", Content: "hi"}})
	s.Submit("tok", "", []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: ""}})
	s.Close()
	if calls != 0 {
		t.Fatalf("sidecar called %d times", calls)
	}
}

func TestSubmitter_DropsOversizedConversationID(t *testing.T) {
	var got submission
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer sidecar.Close()

	s := NewSubmitter(sidecar.URL)
	s.Submit("tok", string(make([]byte, maxConversationIDLen+1)), []Message{{Role: "assistant", Content: "x"}})
	s.Close()
	if got.ConversationID != "" {
		t.Fatal("oversized conversation id must be dropped, not forwarded")
	}
}

func TestSubmitter_NeverBlocksWhenSidecarIsDown(t *testing.T) {
	s := NewSubmitter("http://127.0.0.1:1")
	done := make(chan struct{})
	go func() {
		for i := 0; i < maxPending*2; i++ {
			s.Submit("tok", "", []Message{{Role: "assistant", Content: "x"}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked while the sidecar was unreachable")
	}
	s.Close()
}

func TestSubmitter_NilIsNoop(t *testing.T) {
	var s *Submitter
	s.Submit("tok", "", []Message{{Role: "assistant", Content: "x"}})
	s.Close()
}
