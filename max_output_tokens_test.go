package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

func TestApplyMaxOutputTokensHeader(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		headers []string
		body    string
		// want is the body after the call when no error is expected.
		want      string
		wantError bool
	}{
		{name: "absent header leaves body unchanged", path: "/v1/chat/completions",
			body: `{"model":"m","max_tokens":10}`, want: `{"model":"m","max_tokens":10}`},
		{name: "empty header counts as absent", path: "/v1/chat/completions", headers: []string{" "},
			body: `{"model":"m","max_tokens":10}`, want: `{"model":"m","max_tokens":10}`},
		{name: "non-generation endpoint ignores header", path: "/v1/embeddings", headers: []string{"not-a-number"},
			body: `{"model":"m","input":"hi"}`, want: `{"model":"m","input":"hi"}`},

		{name: "chat without limit gets max_completion_tokens", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m"}`, want: `{"model":"m","max_completion_tokens":64}`},
		{name: "completions without limit gets max_tokens", path: "/v1/completions", headers: []string{"64"},
			body: `{"model":"m"}`, want: `{"model":"m","max_tokens":64}`},
		{name: "responses without limit gets max_output_tokens", path: "/v1/responses", headers: []string{" 64 "},
			body: `{"model":"m"}`, want: `{"model":"m","max_output_tokens":64}`},

		{name: "header replaces larger body limit", path: "/v1/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":1000000}`, want: `{"model":"m","max_tokens":64}`},
		{name: "header replaces smaller body limit", path: "/v1/responses", headers: []string{"64"},
			body: `{"model":"m","max_output_tokens":32}`, want: `{"model":"m","max_output_tokens":64}`},
		{name: "header replaces both chat fields", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":10,"max_completion_tokens":1000000}`, want: `{"model":"m","max_completion_tokens":64}`},
		{name: "header replaces chat max_tokens", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":1000000}`, want: `{"model":"m","max_completion_tokens":64}`},
		{name: "header replaces malformed body limit", path: "/v1/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":"lots"}`, want: `{"model":"m","max_tokens":64}`},

		{name: "non-numeric header is rejected", path: "/v1/chat/completions", headers: []string{"lots"},
			body: `{"model":"m"}`, wantError: true},
		{name: "zero header is rejected", path: "/v1/chat/completions", headers: []string{"0"},
			body: `{"model":"m"}`, wantError: true},
		{name: "negative header is rejected", path: "/v1/chat/completions", headers: []string{"-1"},
			body: `{"model":"m"}`, wantError: true},
		{name: "list header is rejected", path: "/v1/chat/completions", headers: []string{"64, 128"},
			body: `{"model":"m"}`, wantError: true},
		{name: "repeated header is rejected", path: "/v1/chat/completions", headers: []string{"64", "64"},
			body: `{"model":"m"}`, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			for _, value := range tt.headers {
				header.Add(manager.MaxOutputTokensRequestHeader, value)
			}
			body, err := decodeJSONBody([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}

			apiErr := applyMaxOutputTokensHeader(header, tt.path, body)
			if tt.wantError {
				if apiErr == nil {
					t.Fatalf("expected an error, body became %v", body)
				}
				if apiErr.Status != http.StatusBadRequest || apiErr.Type != manager.ErrTypeInvalidRequest {
					t.Fatalf("error = %+v, want 400 %s", apiErr, manager.ErrTypeInvalidRequest)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("unexpected error: %+v", apiErr)
			}
			got, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			want, err := decodeJSONBody([]byte(tt.want))
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, _ := json.Marshal(want)
			if string(got) != string(wantJSON) {
				t.Fatalf("body = %s, want %s", got, wantJSON)
			}
		})
	}
}
