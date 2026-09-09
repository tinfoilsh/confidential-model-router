package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/autoroute"
	"github.com/tinfoilsh/confidential-model-router/manager"
	"github.com/tinfoilsh/confidential-model-router/toolruntime"
)

func TestRateLimitIdentity(t *testing.T) {
	// mkJWT builds a compact-JWS-shaped token (header.payload.sig) with the
	// given JSON payload; the signature is irrelevant here since rateLimitIdentity
	// reads the payload without verifying.
	mkJWT := func(payload string) string {
		enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
		return enc(`{"alg":"EdDSA","typ":"at+jwt"}`) + "." + enc(payload) + ".sig"
	}
	jwtWithSub := mkJWT(`{"sub":"user_42","client_id":"tinfoil-chat"}`)
	jwtNoSub := mkJWT(`{"client_id":"tinfoil-chat"}`)

	tests := []struct {
		name   string
		apiKey string
		want   string
	}{
		{"jwt sub extracted", jwtWithSub, "user_42"},
		{"jwt without sub falls back to bearer", jwtNoSub, jwtNoSub},
		{"opaque token key falls back", "tk_abc123", "tk_abc123"},
		{"chat key falls back", "chat_xyz789", "chat_xyz789"},
		{"non-jwt dotted string falls back", "a.b.c", "a.b.c"},
		{"empty stays empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rateLimitIdentity(tt.apiKey); got != tt.want {
				t.Errorf("rateLimitIdentity(%q) = %q, want %q", tt.apiKey, got, tt.want)
			}
		})
	}
}

func TestLimitRequestBodyRejectsKnownOversize(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	req.ContentLength = maxRequestBodySize + 1
	rec := httptest.NewRecorder()
	if limitRequestBody(rec, req) {
		t.Fatal("oversized request was accepted")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestWriteRequestBodyErrorClassifiesChunkedOversize(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRequestBodyError(rec, &http.MaxBytesError{Limit: maxRequestBodySize})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestModelHeaderMatches(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		bodyModel string
		want      bool
	}{
		{"absent header skips check", "", "gpt-oss-120b", true},
		{"matching header", "gpt-oss-120b", "gpt-oss-120b", true},
		{"surrounding whitespace tolerated", "  gpt-oss-120b ", "gpt-oss-120b", true},
		{"mismatched header", "gpt-oss-120b", "qwen3-tts", false},
		{"header compared against literal auto", "auto", "auto", true},
		{"case sensitive", "GPT-OSS-120B", "gpt-oss-120b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set(manager.ModelRequestHeader, tt.header)
			}
			if got := modelHeaderMatches(h, tt.bodyModel); got != tt.want {
				t.Errorf("modelHeaderMatches(%q, %q) = %v, want %v", tt.header, tt.bodyModel, got, tt.want)
			}
		})
	}
}

func TestExtractModelFromMultipart(t *testing.T) {
	tests := []struct {
		name          string
		model         string
		expectModel   string
		expectDefault bool
	}{
		{
			name:        "voxtral model specified",
			model:       "voxtral-small-24b",
			expectModel: "voxtral-small-24b",
		},
		{
			name:        "whisper model specified",
			model:       "whisper-large-v3-turbo",
			expectModel: "whisper-large-v3-turbo",
		},
		{
			name:          "no model specified",
			model:         "",
			expectModel:   "",
			expectDefault: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create multipart form
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)

			// Add model field if specified
			if tt.model != "" {
				if err := writer.WriteField("model", tt.model); err != nil {
					t.Fatalf("failed to write model field: %v", err)
				}
			}

			// Add a dummy file field (simulating audio file)
			part, err := writer.CreateFormFile("file", "test.wav")
			if err != nil {
				t.Fatalf("failed to create form file: %v", err)
			}
			part.Write([]byte("fake audio data"))

			writer.Close()

			// Create request
			req, err := http.NewRequest("POST", "/v1/audio/transcriptions", body)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			req.Header.Set("Content-Type", writer.FormDataContentType())

			// Extract model
			modelName, bodyBytes, err := extractModelFromMultipart(req)
			if err != nil {
				t.Fatalf("extractModelFromMultipart failed: %v", err)
			}

			// Verify model extraction
			if modelName != tt.expectModel {
				t.Errorf("expected model %q, got %q", tt.expectModel, modelName)
			}

			// Verify body was preserved for forwarding
			if len(bodyBytes) == 0 {
				t.Error("body bytes should not be empty")
			}

			// Simulate what the handler does - apply default if empty
			if modelName == "" {
				modelName = "voxtral-small-24b"
			}

			t.Logf("Extracted model: %s (would route to this enclave)", modelName)
		})
	}
}

func TestBodyPreservedAfterExtraction(t *testing.T) {
	// Create multipart form with model and file
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	writer.WriteField("model", "voxtral-small-24b")
	part, _ := writer.CreateFormFile("file", "test.wav")
	part.Write([]byte("fake audio data for testing"))
	writer.Close()

	originalBody := body.Bytes()

	// Create request
	req, _ := http.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(originalBody))
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Extract model
	_, bodyBytes, err := extractModelFromMultipart(req)
	if err != nil {
		t.Fatalf("extraction failed: %v", err)
	}

	// Verify body is identical
	if !bytes.Equal(bodyBytes, originalBody) {
		t.Error("body bytes should match original body")
	}

	// Verify we can restore it to the request
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	restoredBody, _ := io.ReadAll(req.Body)
	if !bytes.Equal(restoredBody, originalBody) {
		t.Error("restored body should match original")
	}

	t.Log("Body preserved correctly for forwarding to backend")
}

// TestAudioTranscriptionRouting tests the full HTTP handler routing logic for audio endpoints
func TestAudioTranscriptionRouting(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		modelInRequest string
		expectedModel  string
	}{
		{
			name:           "voxtral model routes to voxtral",
			path:           "/v1/audio/transcriptions",
			modelInRequest: "voxtral-small-24b",
			expectedModel:  "voxtral-small-24b",
		},
		{
			name:           "whisper model routes to whisper",
			path:           "/v1/audio/transcriptions",
			modelInRequest: "whisper-large-v3-turbo",
			expectedModel:  "whisper-large-v3-turbo",
		},
		{
			name:           "no model defaults to voxtral",
			path:           "/v1/audio/transcriptions",
			modelInRequest: "",
			expectedModel:  "voxtral-small-24b",
		},
		{
			name:           "other audio path defaults to voxtral",
			path:           "/v1/audio/speech",
			modelInRequest: "",
			expectedModel:  "voxtral-small-24b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Track which model the handler would route to
			var routedModel string

			// Create a test handler that mimics the routing logic
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var modelName string

				// This is the exact logic from main.go for audio paths
				if r.URL.Path == "/v1/audio/transcriptions" || r.URL.Path == "/v1/audio/speech" ||
					len(r.URL.Path) > 10 && r.URL.Path[:10] == "/v1/audio/" {
					var bodyBytes []byte
					var err error
					modelName, bodyBytes, err = extractModelFromMultipart(r)
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					if modelName == "" {
						modelName = "voxtral-small-24b"
					}
					r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				}

				routedModel = modelName

				// In real code, this would forward to the enclave
				// For testing, we just return the model that would be used
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{
					"routed_to_model": modelName,
				})
			})

			// Create multipart request
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)
			if tt.modelInRequest != "" {
				writer.WriteField("model", tt.modelInRequest)
			}
			filePart, _ := writer.CreateFormFile("file", "test.wav")
			filePart.Write([]byte("fake audio data"))
			writer.Close()

			req := httptest.NewRequest("POST", tt.path, body)
			req.Header.Set("Content-Type", writer.FormDataContentType())

			// Execute request
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			// Verify routing
			if rec.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
			}

			if routedModel != tt.expectedModel {
				t.Errorf("expected routing to model %q, got %q", tt.expectedModel, routedModel)
			}

			t.Logf("✓ Request to %s with model=%q routed to: %s", tt.path, tt.modelInRequest, routedModel)
		})
	}
}

// TestJSONRoutingUnchanged verifies that JSON routing (chat completions, embeddings) still works
func TestJSONRoutingUnchanged(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		body          map[string]interface{}
		expectedModel string
	}{
		{
			name: "chat completion extracts model from JSON",
			path: "/v1/chat/completions",
			body: map[string]interface{}{
				"model":    "llama3-3-70b",
				"messages": []interface{}{},
			},
			expectedModel: "llama3-3-70b",
		},
		{
			name: "embeddings extracts model from JSON",
			path: "/v1/embeddings",
			body: map[string]interface{}{
				"model": "nomic-embed-text",
				"input": "test",
			},
			expectedModel: "nomic-embed-text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var routedModel string

			// Create handler that mimics JSON body parsing logic
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				bodyBytes, _ := io.ReadAll(r.Body)
				json.Unmarshal(bodyBytes, &body)

				if model, ok := body["model"].(string); ok {
					routedModel = model
				}

				w.WriteHeader(http.StatusOK)
			})

			bodyBytes, _ := json.Marshal(tt.body)
			req := httptest.NewRequest("POST", tt.path, bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if routedModel != tt.expectedModel {
				t.Errorf("expected model %q, got %q", tt.expectedModel, routedModel)
			}

			t.Logf("✓ JSON request to %s correctly extracts model: %s", tt.path, routedModel)
		})
	}
}

func TestEnsureStreamingUsageOptions(t *testing.T) {
	tests := []struct {
		name                  string
		body                  map[string]interface{}
		wantIncludeUsage      bool
		wantContinuousPresent bool
		wantContinuousUsage   bool
		wantClientUsageHeader bool
	}{
		{
			name:                  "adds include and continuous usage when stream_options missing",
			body:                  map[string]interface{}{"stream": true},
			wantIncludeUsage:      true,
			wantContinuousPresent: true,
			wantContinuousUsage:   true,
		},
		{
			name: "preserves client continuous usage request",
			body: map[string]interface{}{
				"stream": true,
				"stream_options": map[string]interface{}{
					"continuous_usage_stats": true,
				},
			},
			wantIncludeUsage:      true,
			wantContinuousPresent: true,
			wantContinuousUsage:   true,
			wantClientUsageHeader: true,
		},
		{
			name: "marks client include usage request",
			body: map[string]interface{}{
				"stream": true,
				"stream_options": map[string]interface{}{
					"include_usage": true,
				},
			},
			wantIncludeUsage:      true,
			wantContinuousPresent: true,
			wantContinuousUsage:   true,
			wantClientUsageHeader: true,
		},
		{
			name: "does not mark explicit false as client usage request",
			body: map[string]interface{}{
				"stream": true,
				"stream_options": map[string]interface{}{
					"include_usage": false,
				},
			},
			wantIncludeUsage:      true,
			wantContinuousPresent: true,
			wantContinuousUsage:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := make(http.Header)

			ensureStreamingUsageOptions(tt.body, headers)

			streamOptions, ok := tt.body["stream_options"].(map[string]interface{})
			if !ok {
				t.Fatal("stream_options should be present")
			}

			includeUsage, ok := streamOptions["include_usage"].(bool)
			if !ok {
				t.Fatal("include_usage should be a bool")
			}
			if includeUsage != tt.wantIncludeUsage {
				t.Fatalf("include_usage = %v, want %v", includeUsage, tt.wantIncludeUsage)
			}

			continuousUsage, hasContinuous := streamOptions["continuous_usage_stats"].(bool)
			if hasContinuous != tt.wantContinuousPresent {
				t.Fatalf("continuous_usage_stats present = %v, want %v", hasContinuous, tt.wantContinuousPresent)
			}
			if hasContinuous && continuousUsage != tt.wantContinuousUsage {
				t.Fatalf("continuous_usage_stats = %v, want %v", continuousUsage, tt.wantContinuousUsage)
			}

			gotHeader := headers.Get("X-Tinfoil-Client-Requested-Usage") == "true"
			if gotHeader != tt.wantClientUsageHeader {
				t.Fatalf("client usage header = %v, want %v", gotHeader, tt.wantClientUsageHeader)
			}
		})
	}
}

// TestDetectToolProfiles pins the contract between the request shape
// and the set of MCP profiles the router activates. Display-only client
// tools can also enter the tool loop without activating an MCP profile;
// adding a new profile must come with a new case here.
func TestDetectToolProfiles(t *testing.T) {
	profileNames := func(ps []toolruntime.Profile) []string {
		names := make([]string, 0, len(ps))
		for _, p := range ps {
			names = append(names, p.Name)
		}
		return names
	}

	tests := []struct {
		name string
		path string
		body map[string]any
		want []string
	}{
		{
			name: "chat completions with web_search_options",
			path: "/v1/chat/completions",
			body: map[string]any{"web_search_options": map[string]any{}},
			want: []string{"web_search"},
		},
		{
			name: "responses with web_search tool",
			path: "/v1/responses",
			body: map[string]any{"tools": []any{map[string]any{"type": "web_search"}}},
			want: []string{"web_search"},
		},
		{
			name: "responses without any router-owned tool",
			path: "/v1/responses",
			body: map[string]any{"tools": []any{map[string]any{"type": "function", "name": "foo"}}},
			want: nil,
		},
		{
			name: "responses with unknown tool type is ignored",
			path: "/v1/responses",
			body: map[string]any{"tools": []any{map[string]any{"type": "some_future_tool"}}},
			want: nil,
		},
		{
			name: "chat completions without web_search_options",
			path: "/v1/chat/completions",
			body: map[string]any{"messages": []any{}},
			want: nil,
		},
		{
			name: "responses duplicates do not stack web_search",
			path: "/v1/responses",
			body: map[string]any{
				"web_search_options": map[string]any{},
				"tools":              []any{map[string]any{"type": "web_search"}},
			},
			want: []string{"web_search"},
		},
		{
			name: "chat completions with code_execution_options",
			path: "/v1/chat/completions",
			body: map[string]any{"code_execution_options": map[string]any{
				"accessToken":        "a",
				"encryptionKey":      "b",
				"containerAuthToken": "c",
			}},
			want: []string{"code_execution"},
		},
		{
			name: "responses with code_execution tool",
			path: "/v1/responses",
			body: map[string]any{"tools": []any{map[string]any{"type": "code_execution"}}},
			want: []string{"code_execution"},
		},
		{
			name: "both web_search and code_execution",
			path: "/v1/chat/completions",
			body: map[string]any{
				"web_search_options": map[string]any{},
				"code_execution_options": map[string]any{
					"accessToken":        "a",
					"encryptionKey":      "b",
					"containerAuthToken": "c",
				},
			},
			want: []string{"web_search", "code_execution"},
		},
		{
			name: "responses with both tool types",
			path: "/v1/responses",
			body: map[string]any{"tools": []any{
				map[string]any{"type": "web_search"},
				map[string]any{"type": "code_execution"},
			}},
			want: []string{"web_search", "code_execution"},
		},
		{
			name: "responses duplicates do not stack code_execution",
			path: "/v1/responses",
			body: map[string]any{
				"code_execution_options": map[string]any{
					"accessToken":        "a",
					"encryptionKey":      "b",
					"containerAuthToken": "c",
				},
				"tools": []any{map[string]any{"type": "code_execution"}},
			},
			want: []string{"code_execution"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// In production, ExtractRouterOptions runs before
			// detectToolProfiles and strips the *_options fields off
			// body. Mirror that here so the test reflects the real
			// call sequence.
			opts, err := toolruntime.ExtractRouterOptions(tc.body)
			if err != nil {
				t.Fatalf("ExtractRouterOptions: %v", err)
			}
			got := profileNames(toolruntime.DetectProfiles(tc.path, opts, tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("toolruntime.DetectProfiles(%s) = %v, want %v", tc.path, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("toolruntime.DetectProfiles(%s)[%d] = %q, want %q", tc.path, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// fakeCatalog is an autoRouteCatalog backed by fixed models and health.
type fakeCatalog struct {
	models  []autoroute.Model
	healthy map[string]bool
}

func (f fakeCatalog) AutoRouteCatalog() []autoroute.Model { return f.models }
func (f fakeCatalog) HasHealthyEnclave(name string) bool  { return f.healthy[name] }

func autoTestReasoning() *autoroute.Reasoning {
	return &autoroute.Reasoning{
		EffortMap: map[string]string{"low": "low", "medium": "high", "high": "max"},
		Params: map[string]autoroute.EndpointParams{
			"/v1/chat/completions": {
				Enable: map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": "$EFFORT"}},
			},
			"/v1/responses": {
				Enable: map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": "$EFFORT"}},
			},
		},
	}
}

// autoTestCatalog mirrors the shape of the production catalog: a strong
// text-only model, a strong always-on multimodal model, a mid multimodal model
// with efforts, and a tiny text-only model with no reasoning.
func autoTestCatalog() []autoroute.Model {
	return []autoroute.Model{
		{Name: "smart-text", Scores: map[string]int{"low": 39, "medium": 42, "high": 45}, Reasoning: autoTestReasoning()},
		{Name: "smart-vision", Multimodal: true, Scores: map[string]int{"on": 44}},
		{Name: "fast-vision", Multimodal: true, Scores: map[string]int{"low": 36, "medium": 39, "high": 42}, Reasoning: autoTestReasoning()},
		{Name: "tiny-text", Scores: map[string]int{"off": 8}},
	}
}

func allHealthy(models []autoroute.Model) map[string]bool {
	healthy := make(map[string]bool, len(models))
	for _, m := range models {
		healthy[m.Name] = true
	}
	return healthy
}

func TestResolveAutoModel_HeaderPicksModelAndEffort(t *testing.T) {
	catalog := fakeCatalog{models: autoTestCatalog(), healthy: allHealthy(autoTestCatalog())}
	header := http.Header{}
	// Level 87 is the exact fit for smart-text/low (39) and fast-vision/medium
	// (39); nothing else is inside the fit band, so the multimodal fast-vision
	// wins and its native "medium" effort must be applied.
	header.Set(autoroute.IntelligenceHeader, "87")
	body := map[string]any{
		"model":    "auto",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	}

	resolved, err := resolveAutoModel(catalog, header, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("resolveAutoModel: %v", err)
	}
	if resolved != "fast-vision" || body["model"] != "fast-vision" {
		t.Fatalf("resolved = %q, body[model] = %v, want fast-vision", resolved, body["model"])
	}
	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != "high" {
		t.Fatalf("expected native effort high (mapped from medium) for level 87, got %v", kwargs)
	}
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 1 || body["stream"] != true {
		t.Fatalf("unrelated body fields were clobbered: %v", body)
	}
}

func TestResolveAutoModel_BodyOptionsWinOverHeader(t *testing.T) {
	catalog := fakeCatalog{models: autoTestCatalog(), healthy: allHealthy(autoTestCatalog())}
	header := http.Header{}
	header.Set(autoroute.IntelligenceHeader, "100")
	body := map[string]any{
		"model":              "auto",
		"auto_model_options": map[string]any{"intelligence": float64(0)},
	}

	resolved, err := resolveAutoModel(catalog, header, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("resolveAutoModel: %v", err)
	}
	if resolved != "tiny-text" {
		t.Fatalf("resolved = %q, want tiny-text for level 0", resolved)
	}
	if _, ok := body["auto_model_options"]; ok {
		t.Fatal("auto_model_options must be stripped from the body")
	}
}

func TestResolveAutoModel_DefaultLevel(t *testing.T) {
	catalog := fakeCatalog{models: autoTestCatalog(), healthy: allHealthy(autoTestCatalog())}
	body := map[string]any{"model": "auto"}

	resolved, err := resolveAutoModel(catalog, http.Header{}, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("resolveAutoModel: %v", err)
	}
	// Catalog max is 45, so fast-vision/low normalizes to 80 and tiny-text/off
	// to 18; the default target of 50 is nearer the former.
	if resolved != "fast-vision" {
		t.Fatalf("resolved = %q, want fast-vision for the default level", resolved)
	}
	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != "low" {
		t.Fatalf("expected effort low, got %v", kwargs)
	}
}

func TestResolveAutoModel_FallsBackWhenBestFitUnhealthy(t *testing.T) {
	models := autoTestCatalog()
	healthy := allHealthy(models)
	healthy["smart-vision"] = false
	catalog := fakeCatalog{models: models, healthy: healthy}
	header := http.Header{}
	header.Set(autoroute.IntelligenceHeader, "100")
	body := map[string]any{"model": "auto"}

	resolved, err := resolveAutoModel(catalog, header, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("resolveAutoModel: %v", err)
	}
	if resolved != "smart-text" {
		t.Fatalf("resolved = %q, want smart-text as next best when smart-vision is down", resolved)
	}
	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != "max" {
		t.Fatalf("expected native effort max for smart-text/high, got %v", kwargs)
	}
}

func TestResolveAutoModel_NothingHealthyStillResolvesBestFit(t *testing.T) {
	catalog := fakeCatalog{models: autoTestCatalog(), healthy: map[string]bool{}}
	header := http.Header{}
	header.Set(autoroute.IntelligenceHeader, "100")
	body := map[string]any{"model": "auto"}

	resolved, err := resolveAutoModel(catalog, header, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("resolveAutoModel: %v", err)
	}
	if resolved != "smart-vision" {
		t.Fatalf("resolved = %q, want best-fit smart-vision so serving surfaces the outage", resolved)
	}
	if _, ok := body["chat_template_kwargs"]; ok {
		t.Fatal("model without reasoning params must not receive kwargs")
	}
}

func TestResolveAutoModel_VisualInputRequiresMultimodal(t *testing.T) {
	catalog := fakeCatalog{models: autoTestCatalog(), healthy: allHealthy(autoTestCatalog())}
	header := http.Header{}
	header.Set(autoroute.IntelligenceHeader, "100")
	body := map[string]any{
		"model": "auto",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is this?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}},
		}}},
	}

	resolved, err := resolveAutoModel(catalog, header, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("resolveAutoModel: %v", err)
	}
	if resolved != "smart-vision" {
		t.Fatalf("resolved = %q, want smart-vision (text-only smart-text must be excluded)", resolved)
	}
}

func TestResolveAutoModel_VisualInputWithoutMultimodalModels(t *testing.T) {
	models := []autoroute.Model{{Name: "text-only", Scores: map[string]int{"off": 20}}}
	catalog := fakeCatalog{models: models, healthy: allHealthy(models)}
	body := map[string]any{
		"model": "auto",
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "https://x/y.png"}}}},
	}

	if _, err := resolveAutoModel(catalog, http.Header{}, "/v1/responses", body); err == nil {
		t.Fatal("expected error when an image request has no multimodal candidates")
	}
}

func TestResolveAutoModel_RouterEffortOverridesClient(t *testing.T) {
	catalog := fakeCatalog{models: autoTestCatalog(), healthy: allHealthy(autoTestCatalog())}
	header := http.Header{}
	// Level 87 resolves to fast-vision/medium, whose native effort is "high".
	header.Set(autoroute.IntelligenceHeader, "87")
	body := map[string]any{
		"model":                "auto",
		"chat_template_kwargs": map[string]any{"reasoning_effort": "low", "keep": "me"},
	}

	if _, err := resolveAutoModel(catalog, header, "/v1/responses", body); err != nil {
		t.Fatalf("resolveAutoModel: %v", err)
	}
	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != "high" || kwargs["keep"] != "me" {
		t.Fatalf("router effort must win and siblings survive, got %v", kwargs)
	}
}

func TestResolveAutoModel_InvalidLevel(t *testing.T) {
	catalog := fakeCatalog{models: autoTestCatalog(), healthy: allHealthy(autoTestCatalog())}
	header := http.Header{}
	header.Set(autoroute.IntelligenceHeader, "101")
	if _, err := resolveAutoModel(catalog, header, "/v1/chat/completions", map[string]any{"model": "auto"}); err == nil {
		t.Fatal("expected error for out-of-range intelligence header")
	}

	body := map[string]any{"model": "auto", "auto_model_options": map[string]any{"intelligence": "high"}}
	if _, err := resolveAutoModel(catalog, http.Header{}, "/v1/chat/completions", body); err == nil {
		t.Fatal("expected error for non-numeric body intelligence")
	}
	if _, ok := body["auto_model_options"]; ok {
		t.Fatal("auto_model_options must be stripped even on error")
	}
}

func TestResolveAutoModel_LegacyArrayIgnored(t *testing.T) {
	catalog := fakeCatalog{models: autoTestCatalog(), healthy: allHealthy(autoTestCatalog())}
	header := http.Header{}
	header.Set(autoroute.IntelligenceHeader, "100")
	body := map[string]any{
		"model": "auto",
		"auto_model_options": []any{
			map[string]any{"model": "tiny-text", "params": map[string]any{"reasoning_effort": "high"}},
		},
	}

	resolved, err := resolveAutoModel(catalog, header, "/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("resolveAutoModel: %v", err)
	}
	if resolved != "smart-vision" {
		t.Fatalf("resolved = %q, want smart-vision; legacy candidate list must not steer routing", resolved)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatal("legacy per-candidate params must not be merged into the body")
	}
	if _, ok := body["auto_model_options"]; ok {
		t.Fatal("auto_model_options must be stripped from the body")
	}
}

func TestResolveAutoModel_EmptyCatalog(t *testing.T) {
	catalog := fakeCatalog{}
	if _, err := resolveAutoModel(catalog, http.Header{}, "/v1/chat/completions", map[string]any{"model": "auto"}); err == nil {
		t.Fatal("expected error when no model publishes intelligence scores")
	}
}

// The Responses input-token route derives chat_template_kwargs from the
// client's reasoning.effort; for model "auto" the router's chosen effort must
// be what reaches the tokenizer, not the client's.
func TestHandleInputTokens_AutoUsesRouterEffort(t *testing.T) {
	catalog := fakeCatalog{models: autoTestCatalog(), healthy: allHealthy(autoTestCatalog())}
	countTokens := func(t *testing.T, level string) (string, map[string]any) {
		t.Helper()
		var dispatchedModel string
		var dispatchedBody map[string]any
		dispatch := func(_ context.Context, model, _ string, body []byte, _ http.Header) (*http.Response, error) {
			dispatchedModel = model
			if err := json.Unmarshal(body, &dispatchedBody); err != nil {
				t.Fatal(err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"count":5}`)),
			}, nil
		}
		req := httptest.NewRequest(http.MethodPost, responsesInputTokensPath, strings.NewReader(`{
			"model":"auto",
			"reasoning":{"effort":"high"},
			"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]
		}`))
		req.Header.Set(autoroute.IntelligenceHeader, level)
		rec := httptest.NewRecorder()
		handleInputTokens(rec, req, "secret-key", "", func(body map[string]any) (string, error) {
			return resolveAutoModel(catalog, req.Header, inputTokensCompletionPath(req.URL.Path), body)
		}, dispatch)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		return dispatchedModel, dispatchedBody
	}

	// Level 50 lands on fast-vision/low; its native "low" must reach the
	// tokenizer instead of the client's "high".
	model, body := countTokens(t, "50")
	if model != "fast-vision" || body["model"] != "fast-vision" {
		t.Fatalf("expected tokenization against fast-vision, got %q / %v", model, body["model"])
	}
	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != "low" {
		t.Fatalf("router effort must reach the tokenizer, got %v", body["chat_template_kwargs"])
	}

	// Level 0 lands on tiny-text, which has no reasoning; the client's effort
	// must not be re-derived into kwargs for it either.
	model, body = countTokens(t, "0")
	if model != "tiny-text" {
		t.Fatalf("expected tokenization against tiny-text, got %q", model)
	}
	if kwargs, ok := body["chat_template_kwargs"]; ok {
		t.Fatalf("client reasoning.effort must not reach the tokenizer for a model without reasoning, got %v", kwargs)
	}
}

func TestFilterModelsToServed(t *testing.T) {
	served := map[string]*manager.Model{
		"kimi-k2-6": {},
		"glm-5-2":   {},
	}

	body := []byte(`{"object":"list","data":[
		{"id":"kimi-k2-6","name":"Kimi K2.6","type":"chat"},
		{"id":"deepseek-v4-pro","name":"DeepSeek V4 Pro","type":"chat"},
		{"id":"glm-5-2","name":"GLM-5.2","type":"chat"}
	]}`)

	out, err := filterModelsToServed(body, served)
	if err != nil {
		t.Fatalf("unexpected error for a well-formed payload: %v", err)
	}

	var parsed struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal filtered output: %v", err)
	}
	if parsed.Object != "list" {
		t.Errorf("object = %q, want %q", parsed.Object, "list")
	}

	gotIDs := make(map[string]map[string]any)
	for _, m := range parsed.Data {
		gotIDs[m["id"].(string)] = m
	}
	if len(gotIDs) != 2 {
		t.Fatalf("kept %d models, want 2: %v", len(gotIDs), gotIDs)
	}
	if _, ok := gotIDs["deepseek-v4-pro"]; ok {
		t.Error("deepseek-v4-pro is not served but was kept")
	}
	// Passthrough fields (name) must survive filtering.
	if kimi := gotIDs["kimi-k2-6"]; kimi == nil || kimi["name"] != "Kimi K2.6" {
		t.Errorf("kimi-k2-6 name not preserved: %v", kimi)
	}
}

func TestFilterModelsToServedRejectsMalformed(t *testing.T) {
	if _, err := filterModelsToServed([]byte(`{bad json`), map[string]*manager.Model{}); err == nil {
		t.Error("expected an error for a malformed payload so the caller can surface it")
	}
}
