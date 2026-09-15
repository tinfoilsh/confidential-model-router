package safeguards

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type fakeSidecar struct {
	*httptest.Server
	mu   sync.Mutex
	subs []submission
}

func newFakeSidecar(t *testing.T) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sub submission
		if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.subs = append(f.subs, sub)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(f.Close)
	return f
}

func isChatToken(credential string) bool { return credential == "chat-jwt" }

// handle runs one request through Observe the way main.go does: wrap, parse
// the body into history, write the upstream reply, then finish.
func handle(t *testing.T, s *Submitter, path, auth, conversationID, reqBody, contentType, reply string) (*httptest.ResponseRecorder, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(reqBody))
	req.Header.Set("Authorization", auth)
	if conversationID != "" {
		req.Header.Set(ConversationIDHeader, conversationID)
	}

	w, capture, finish := s.Observe(rec, req, isChatToken)
	var body map[string]any
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if capture != nil {
		capture.SetMessages(RequestMessages(path, body))
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(reply))
	finish()
	return rec, req.Header
}

func TestObserve_SubmitsChatConversationWithReply(t *testing.T) {
	sidecar := newFakeSidecar(t)
	s := NewSubmitter(sidecar.URL)
	rec, upstreamHeaders := handle(t, s,
		"/v1/chat/completions", "Bearer chat-jwt", "chat-7",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		"application/json",
		`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`,
	)
	s.Close()

	if upstreamHeaders.Get(ConversationIDHeader) != "" {
		t.Fatal("conversation id header must be stripped before the request is forwarded")
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatal("client response must be unchanged")
	}
	if len(sidecar.subs) != 1 {
		t.Fatalf("submissions = %d, want 1", len(sidecar.subs))
	}
	got := sidecar.subs[0]
	want := submission{Credential: "chat-jwt", ConversationID: "chat-7", Messages: []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestObserve_ResponsesStreaming(t *testing.T) {
	sidecar := newFakeSidecar(t)
	s := NewSubmitter(sidecar.URL)
	handle(t, s,
		"/v1/responses", "Bearer chat-jwt", "",
		`{"model":"m","instructions":"be brief","input":"question"}`,
		"text/event-stream",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ans\"}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"wer\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n",
	)
	s.Close()

	if len(sidecar.subs) != 1 {
		t.Fatalf("submissions = %d, want 1", len(sidecar.subs))
	}
	msgs := sidecar.subs[0].Messages
	want := []Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "question"}, {Role: "assistant", Content: "answer"}}
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("got %+v", msgs)
	}
}

func TestObserve_SkipsEmptyHistory(t *testing.T) {
	sidecar := newFakeSidecar(t)
	s := NewSubmitter(sidecar.URL)
	handle(t, s, "/v1/chat/completions", "Bearer chat-jwt", "chat-1",
		`{"messages":[]}`, "application/json", `{"choices":[{"message":{"content":"reply"}}]}`)
	s.Close()
	if len(sidecar.subs) != 0 {
		t.Fatal("empty history must not produce a submission")
	}
}

func TestObserve_SkipsCanceledAndPanickingRequests(t *testing.T) {
	for _, abort := range []string{"cancel", "panic"} {
		t.Run(abort, func(t *testing.T) {
			sidecar := newFakeSidecar(t)
			s := NewSubmitter(sidecar.URL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer chat-jwt")
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				w, capture, finish := s.Observe(httptest.NewRecorder(), req, isChatToken)
				defer finish()
				capture.SetMessages([]Message{{Role: "user", Content: "hi"}})
				w.Write([]byte(`{"choices":[{"message":{"content":"reply"}}]}`))
				if abort == "panic" {
					panic(http.ErrAbortHandler)
				}
				cancel()
			}()
			s.Close()
			if abort == "panic" && recovered != http.ErrAbortHandler {
				t.Fatalf("panic was not preserved: %v", recovered)
			}
			if len(sidecar.subs) != 0 {
				t.Fatal("aborted request must not produce a submission")
			}
		})
	}
}

func TestObserve_SkipsIneligibleRequests(t *testing.T) {
	sidecar := newFakeSidecar(t)
	s := NewSubmitter(sidecar.URL)
	for name, tc := range map[string]struct{ path, auth string }{
		"api key":          {"/v1/chat/completions", "Bearer tk_live_apikey"},
		"non-chat path":    {"/v1/embeddings", "Bearer chat-jwt"},
		"missing auth":     {"/v1/chat/completions", ""},
		"malformed bearer": {"/v1/chat/completions", "chat-jwt"},
	} {
		rec, _ := handle(t, s, tc.path, tc.auth, "chat-1",
			`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			"application/json",
			`{"choices":[{"message":{"content":"hello"}}]}`,
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: response must be unaffected", name)
		}
	}
	s.Close()
	if len(sidecar.subs) != 0 {
		t.Fatalf("ineligible requests were submitted: %+v", sidecar.subs)
	}
}

func TestObserve_NilSubmitterStillStripsHeader(t *testing.T) {
	var s *Submitter
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(ConversationIDHeader, "chat-1")
	req.Header.Set("Authorization", "Bearer chat-jwt")
	w, capture, finish := s.Observe(httptest.NewRecorder(), req, isChatToken)
	capture.SetMessages(nil)
	finish()
	if req.Header.Get(ConversationIDHeader) != "" {
		t.Fatal("header must be stripped even when the sidecar is not configured")
	}
	if _, wrapped := w.(*Capture); wrapped {
		t.Fatal("writer must not be wrapped when the sidecar is not configured")
	}
}
