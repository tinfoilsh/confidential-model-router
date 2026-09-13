package safeguards

import (
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

func TestCapture_ResponsesSSE_DeltasWhenNoCompleted(t *testing.T) {
	c := NewCapture(httptest.NewRecorder())
	writeAll(t, c, "text/event-stream", 200,
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n",
		"event: response.refusal.delta\ndata: {\"type\":\"response.refusal.delta\",\"delta\":\"b\"}\n\n",
	)
	if got := c.Text(); got != "ab" {
		t.Fatalf("got %q", got)
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
