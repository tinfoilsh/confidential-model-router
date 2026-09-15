package safeguards

import (
	"bufio"
	"bytes"
	"encoding/json"
	"mime"
	"net"
	"net/http"
	"strings"
)

// maxCaptureBytes bounds the assistant text retained per request. Beyond it
// the capture is abandoned: the sidecar has its own transcript cap and the
// classifier only needs the recent turns anyway.
const maxCaptureBytes = 1 << 20

// Paths are the conversational endpoints whose completed turns are classified.
var Paths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/responses":        true,
}

// Capture observes the bytes written to the client and reassembles the
// assistant's reply. It handles the three shapes the router emits: a JSON
// body, chat-completions SSE (delta.content per chunk), and Responses SSE
// (response.output_text.delta events, or the final response.completed).
type Capture struct {
	http.ResponseWriter
	messages  []Message
	started   bool
	status    int
	streaming bool
	body      bytes.Buffer
	line      bytes.Buffer
	text      strings.Builder
	completed string
	overflow  bool
	failed    bool
	finished  bool
	terminal  bool
}

// Observe decides whether a request is eligible for classification and, if
// so, wraps w so the reply can be captured. Only first-party chat access
// tokens on conversational paths qualify; API keys are never classified. The
// client's conversation-id header is consumed here so it never reaches an
// upstream model. The returned finish submits the conversation once the
// handler has written its response; the caller must defer it.
func (s *Submitter) Observe(w http.ResponseWriter, r *http.Request, eligible func(credential string) bool) (http.ResponseWriter, *Capture, func()) {
	conversationID := r.Header.Get(ConversationIDHeader)
	r.Header.Del(ConversationIDHeader)
	credential := bearerToken(r.Header.Get("Authorization"))
	if s == nil || !Paths[r.URL.Path] || !eligible(credential) {
		return w, nil, func() {}
	}
	c := &Capture{ResponseWriter: w}
	return c, c, func() {
		if failure := recover(); failure != nil {
			panic(failure)
		}
		if r.Context().Err() != nil {
			return
		}
		if reply := c.Text(); len(c.messages) > 0 && reply != "" {
			s.Submit(credential, conversationID, append(c.messages, Message{Role: "assistant", Content: reply}))
		}
	}
}

// SetMessages records the request history once the body has been parsed. A
// nil Capture is a no-op so callers on ineligible requests need no branch.
func (c *Capture) SetMessages(messages []Message) {
	if c != nil {
		if !fitsSubmission("", "", messages) {
			c.overflow = true
			c.messages = nil
			submissionsTotal.WithLabelValues("oversized").Inc()
			return
		}
		c.messages = messages
	}
}

func bearerToken(header string) string {
	const prefix = "bearer "
	if len(header) >= len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

func (c *Capture) WriteHeader(code int) {
	c.start(code)
	c.ResponseWriter.WriteHeader(code)
}

func (c *Capture) Write(p []byte) (int, error) {
	c.start(http.StatusOK)
	n, err := c.ResponseWriter.Write(p)
	if err != nil || n != len(p) {
		c.failed = true
	}
	if !c.overflow && n > 0 {
		c.observe(p[:n])
	}
	return n, err
}

// start records the status and body shape on the first header or body
// write, mirroring net/http's implicit 200 on a bare Write.
func (c *Capture) start(code int) {
	if code >= http.StatusContinue && code < http.StatusOK && code != http.StatusSwitchingProtocols {
		return
	}
	if c.started {
		return
	}
	c.started = true
	c.status = code
	mediaType, _, _ := mime.ParseMediaType(c.Header().Get("Content-Type"))
	c.streaming = mediaType == "text/event-stream"
}

func (c *Capture) observe(p []byte) {
	if !c.streaming {
		if c.body.Len()+len(p) > maxCaptureBytes {
			c.overflow = true
			return
		}
		c.body.Write(p)
		return
	}
	for len(p) > 0 && !c.overflow {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if c.line.Len()+len(p) > maxCaptureBytes {
				c.overflow = true
				return
			}
			c.line.Write(p)
			return
		}
		if c.line.Len()+i > maxCaptureBytes {
			c.overflow = true
			return
		}
		c.line.Write(p[:i])
		c.observeLine(c.line.Bytes())
		c.line.Reset()
		p = p[i+1:]
	}
}

func (c *Capture) observeLine(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		c.terminal = c.finished
		return
	}
	if len(data) == 0 {
		return
	}
	var frame struct {
		Type     string          `json:"type"`
		Delta    any             `json:"delta"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Output any `json:"output"`
		} `json:"response"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &frame) != nil {
		c.failed = true
		return
	}
	if len(frame.Error) > 0 && string(frame.Error) != "null" {
		c.failed = true
		return
	}
	switch frame.Type {
	case "error", "response.failed", "response.incomplete":
		c.failed = true
	case "response.output_text.delta", "response.refusal.delta":
		if s, ok := frame.Delta.(string); ok {
			c.append(s)
		}
	case "response.completed":
		text := ResponsesOutputText(frame.Response.Output)
		if len(text) > maxCaptureBytes {
			c.overflow = true
			return
		}
		c.completed = text
		c.finished = true
		c.terminal = true
	case "":
		for _, choice := range frame.Choices {
			c.append(choice.Delta.Content)
			c.append(choice.Delta.Refusal)
			if choice.FinishReason != "" {
				c.finished = true
			}
		}
	}
}

func (c *Capture) append(s string) {
	if c.text.Len()+len(s) > maxCaptureBytes {
		c.overflow = true
		return
	}
	c.text.WriteString(s)
}

// Text returns the assistant's reply, or "" when the response was not a
// successful completion or the capture was abandoned.
func (c *Capture) Text() string {
	if c.overflow || c.failed || c.status != http.StatusOK {
		return ""
	}
	if c.streaming {
		if !c.terminal {
			return ""
		}
		if c.completed != "" {
			return c.completed
		}
		return c.text.String()
	}
	var body struct {
		Output  any `json:"output"`
		Choices []struct {
			Message struct {
				Content any    `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(c.body.Bytes(), &body) != nil {
		return ""
	}
	if body.Output != nil {
		return ResponsesOutputText(body.Output)
	}
	if len(body.Choices) == 0 {
		return ""
	}
	if text := flattenContent(body.Choices[0].Message.Content, "text"); text != "" {
		return text
	}
	return body.Choices[0].Message.Refusal
}

func (c *Capture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		c.start(http.StatusOK)
		f.Flush()
	}
}

func (c *Capture) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := c.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (c *Capture) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}
