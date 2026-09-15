package safeguards

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func NewCapture(w http.ResponseWriter) *Capture {
	return &Capture{ResponseWriter: w}
}

func writeAll(t *testing.T, c *Capture, contentType string, status int, chunks ...string) *httptest.ResponseRecorder {
	t.Helper()
	c.Header().Set("Content-Type", contentType)
	c.WriteHeader(status)
	for _, chunk := range chunks {
		if _, err := c.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	return c.ResponseWriter.(*httptest.ResponseRecorder)
}

func TestCapture_ChatJSON(t *testing.T) {
	c := NewCapture(httptest.NewRecorder())
	body := `{"choices":[{"message":{"role":"assistant","content":"The answer is 4."}}]}`
	rec := writeAll(t, c, "application/json", 200, body)
	if got := c.Text(); got != "The answer is 4." {
		t.Fatalf("got %q", got)
	}
	if rec.Body.String() != body {
		t.Fatal("capture must pass the body through unchanged")
	}
}

func TestCapture_ChatRefusalJSON(t *testing.T) {
	c := NewCapture(httptest.NewRecorder())
	writeAll(t, c, "application/json", 200, `{"choices":[{"message":{"role":"assistant","content":null,"refusal":"I can't help with that."}}]}`)
	if got := c.Text(); got != "I can't help with that." {
		t.Fatalf("got %q", got)
	}
}

func TestCapture_ChatSSE_SplitAcrossWrites(t *testing.T) {
	c := NewCapture(httptest.NewRecorder())
	frames := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo, \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"wörld\"}}],\"usage\":null}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	// Feed in awkward slices so frames straddle Write calls.
	var chunks []string
	for i := 0; i < len(frames); i += 7 {
		chunks = append(chunks, frames[i:min(i+7, len(frames))])
	}
	rec := writeAll(t, c, "text/event-stream", 200, chunks...)
	if got := c.Text(); got != "Hello, wörld" {
		t.Fatalf("got %q", got)
	}
	if rec.Body.String() != frames {
		t.Fatal("capture must pass the stream through unchanged")
	}
}

func TestCapture_ResponsesSSE_PrefersCompleted(t *testing.T) {
	c := NewCapture(httptest.NewRecorder())
	writeAll(t, c, "text/event-stream", 200,
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"final text\"}]}]}}\n\n",
	)
	if got := c.Text(); got != "final text" {
		t.Fatalf("got %q", got)
	}
}

func TestCapture_ResponsesSSE_RejectsIncompleteStream(t *testing.T) {
	c := NewCapture(httptest.NewRecorder())
	writeAll(t, c, "text/event-stream", 200,
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n",
		"event: response.refusal.delta\ndata: {\"type\":\"response.refusal.delta\",\"delta\":\"b\"}\n\n",
	)
	if got := c.Text(); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestCapture_MediaType(t *testing.T) {
	for _, contentType := range []string{"text/event-stream", "TEXT/EVENT-STREAM; charset=utf-8", "application/text/event-stream"} {
		t.Run(contentType, func(t *testing.T) {
			c := NewCapture(httptest.NewRecorder())
			writeAll(t, c, contentType, http.StatusOK,
				"data: {\"choices\":[{\"delta\":{\"content\":\"reply\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			want := "reply"
			if contentType == "application/text/event-stream" {
				want = ""
			}
			if got := c.Text(); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestCapture_InformationalHeaders(t *testing.T) {
	captured := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := NewCapture(w)
		c.WriteHeader(http.StatusEarlyHints)
		c.WriteHeader(http.StatusContinue)
		c.Header().Set("Content-Type", "application/json")
		c.WriteHeader(http.StatusOK)
		c.Write([]byte(`{"choices":[{"message":{"content":"reply"}}]}`))
		captured <- c.Text()
	}))
	defer server.Close()
	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client received %d", resp.StatusCode)
	}
	if got := <-captured; got != "reply" {
		t.Fatalf("informational status latched: %q", got)
	}
	c := NewCapture(httptest.NewRecorder())
	c.WriteHeader(http.StatusSwitchingProtocols)
	c.WriteHeader(http.StatusOK)
	if c.status != http.StatusSwitchingProtocols {
		t.Fatal("101 must remain terminal")
	}
}

func TestCapture_FlushCommitsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	c := NewCapture(rec)
	c.Header().Set("Content-Type", "application/json")
	c.Flush()
	c.WriteHeader(http.StatusInternalServerError)
	c.Write([]byte(`{"choices":[{"message":{"content":"reply"}}]}`))
	if !rec.Flushed || rec.Code != http.StatusOK || c.Text() != "reply" {
		t.Fatal("flush must commit the implicit 200 for both writer and capture")
	}
}

type shortWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w shortWriter) Write(p []byte) (int, error) {
	n, _ := w.ResponseRecorder.Write(p[:len(p)/2])
	return n, w.err
}

func TestCapture_RecordsOnlyDeliveredBytes(t *testing.T) {
	for _, writeErr := range []error{nil, io.ErrClosedPipe} {
		c := NewCapture(shortWriter{httptest.NewRecorder(), writeErr})
		payload := []byte(`{"choices":[{"message":{"content":"reply"}}]}`)
		n, err := c.Write(payload)
		if n != len(payload)/2 || err != writeErr || c.body.String() != string(payload[:n]) {
			t.Fatal("capture must preserve write results and only record delivered bytes")
		}
		if c.Text() != "" {
			t.Fatal("short or failed writes must not be submitted")
		}
	}
}

func TestCapture_ChatStreamCompletionAndRefusal(t *testing.T) {
	const content = "data: {\"choices\":[{\"delta\":{\"refusal\":\"No.\"}}]}\n\n"
	for _, tc := range []struct{ name, ending, want string }{
		{"stop", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", "No."},
		{"length", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", "No."},
		{"truncated", "", ""},
		{"done without finish", "data: [DONE]\n\n", ""},
		{"error", "data: {\"error\":{\"message\":\"failed\"}}\n\ndata: [DONE]\n\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCapture(httptest.NewRecorder())
			writeAll(t, c, "text/event-stream", http.StatusOK, content, tc.ending)
			if got := c.Text(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCapture_CompletedSnapshotCannotExceedLimit(t *testing.T) {
	frame, err := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{"output": []any{map[string]any{
			"type": "message", "content": []any{map[string]any{"type": "output_text", "text": strings.Repeat("x", maxCaptureBytes+1)}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := "data: " + string(frame) + "\n\n"
	for _, chunks := range [][]string{{wire}, {wire[:len(wire)/2], wire[len(wire)/2:]}} {
		c := NewCapture(httptest.NewRecorder())
		rec := writeAll(t, c, "text/event-stream", http.StatusOK, chunks...)
		if !c.overflow || c.Text() != "" || rec.Body.String() != wire {
			t.Fatal("oversized snapshot must be skipped without changing the response")
		}
	}
}

func TestCapture_ResponsesJSON(t *testing.T) {
	c := NewCapture(httptest.NewRecorder())
	writeAll(t, c, "application/json", 200, `{"object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
	if got := c.Text(); got != "done" {
		t.Fatalf("got %q", got)
	}
}

func TestCapture_ImplicitStatusOK(t *testing.T) {
	c := NewCapture(httptest.NewRecorder())
	c.Header().Set("Content-Type", "application/json")
	c.Write([]byte(`{"choices":[{"message":{"content":"implicit"}}]}`))
	if got := c.Text(); got != "implicit" {
		t.Fatalf("got %q", got)
	}
}

func TestCapture_IgnoresErrorsAndOverflow(t *testing.T) {
	c := NewCapture(httptest.NewRecorder())
	writeAll(t, c, "application/json", http.StatusBadGateway, `{"choices":[{"message":{"content":"never"}}]}`)
	if c.Text() != "" {
		t.Fatal("non-200 responses must not be captured")
	}

	c = NewCapture(httptest.NewRecorder())
	rec := writeAll(t, c, "text/event-stream", 200,
		"data: {\"choices\":[{\"delta\":{\"content\":\""+strings.Repeat("x", maxCaptureBytes)+"\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"tail\"}}]}\n\n",
	)
	if c.Text() != "" {
		t.Fatal("overflowed capture must be abandoned")
	}
	if rec.Body.Len() < maxCaptureBytes {
		t.Fatal("client must still receive the full stream")
	}
}
