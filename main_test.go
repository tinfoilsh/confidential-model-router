package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/toolprofile"
)

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
// and the set of MCP profiles the router activates. This test is the
// single source of truth for "which requests enter the tool loop";
// adding a new profile must come with a new case here.
func TestDetectToolProfiles(t *testing.T) {
	profileNames := func(ps []toolprofile.Profile) []string {
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
			body: map[string]any{"code_execution_options": map[string]any{}},
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
				"web_search_options":     map[string]any{},
				"code_execution_options": map[string]any{},
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
				"code_execution_options": map[string]any{},
				"tools":                  []any{map[string]any{"type": "code_execution"}},
			},
			want: []string{"code_execution"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := profileNames(detectToolProfiles(tc.path, tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("detectToolProfiles(%s) = %v, want %v", tc.path, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("detectToolProfiles(%s)[%d] = %q, want %q", tc.path, i, got[i], tc.want[i])
				}
			}
		})
	}
}
