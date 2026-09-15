package safeguards

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestSubmitter_RejectsOversizedPayloads(t *testing.T) {
	for _, tc := range []struct {
		name       string
		credential string
		messages   []Message
	}{
		{"history", "token", []Message{{Role: "user", Content: strings.Repeat("x", maxSubmissionBytes)}, {Role: "assistant", Content: "reply"}}},
		{"escaping", "token", []Message{{Role: "assistant", Content: strings.Repeat("\x00", maxSubmissionBytes/2)}}},
		{"credential", strings.Repeat("x", maxSubmissionBytes), []Message{{Role: "assistant", Content: "reply"}}},
		{"many messages", "token", append(make([]Message, maxSubmissionBytes/messageJSONOverhead), Message{Role: "assistant", Content: "reply"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Submitter{pending: make(chan []byte, maxPending)}
			s.Submit(tc.credential, "chat", tc.messages)
			if len(s.pending) != 0 {
				t.Fatal("oversized payload must not enter the queue")
			}
		})
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
