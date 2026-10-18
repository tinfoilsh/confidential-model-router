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
		wantCode  string
		wantParam string
		wantError bool
	}{
		{name: "absent header leaves body unchanged", path: "/v1/chat/completions",
			body: `{"model":"m","max_tokens":10}`, want: `{"model":"m","max_tokens":10}`},
		{name: "empty header counts as absent", path: "/v1/chat/completions", headers: []string{" "},
			body: `{"model":"m"}`, want: `{"model":"m"}`},
		{name: "non-generation endpoint ignores header", path: "/v1/embeddings", headers: []string{"not-a-number"},
			body: `{"model":"m","input":"hi"}`, want: `{"model":"m","input":"hi"}`},

		{name: "chat without limit gets max_completion_tokens", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m"}`, want: `{"model":"m","max_completion_tokens":64}`},
		{name: "completions without limit gets max_tokens", path: "/v1/completions", headers: []string{"64"},
			body: `{"model":"m"}`, want: `{"model":"m","max_tokens":64}`},
		{name: "responses without limit gets max_output_tokens", path: "/v1/responses", headers: []string{" 64 "},
			body: `{"model":"m"}`, want: `{"model":"m","max_output_tokens":64}`},
		{name: "null limit is replaced", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":null}`, want: `{"model":"m","max_completion_tokens":64,"max_tokens":null}`},

		{name: "matching max_tokens is kept", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":64}`, want: `{"model":"m","max_tokens":64}`},
		{name: "matching chat fields are kept", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":64,"max_completion_tokens":64}`,
			want: `{"model":"m","max_tokens":64,"max_completion_tokens":64}`},
		{name: "matching max_output_tokens is kept", path: "/v1/responses", headers: []string{"64"},
			body: `{"model":"m","max_output_tokens":64}`, want: `{"model":"m","max_output_tokens":64}`},

		{name: "larger body limit is rejected", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":1000000}`, wantError: true, wantCode: manager.ErrCodeMaxTokensMismatch, wantParam: "max_tokens"},
		{name: "smaller body limit is rejected", path: "/v1/responses", headers: []string{"64"},
			body: `{"model":"m","max_output_tokens":32}`, wantError: true, wantCode: manager.ErrCodeMaxTokensMismatch, wantParam: "max_output_tokens"},
		{name: "engine-preferred field cannot bypass header", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":64,"max_completion_tokens":1000000}`, wantError: true, wantCode: manager.ErrCodeMaxTokensMismatch, wantParam: "max_completion_tokens"},
		{name: "fallback field cannot disagree", path: "/v1/chat/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":1000000,"max_completion_tokens":64}`, wantError: true, wantCode: manager.ErrCodeMaxTokensMismatch, wantParam: "max_tokens"},
		{name: "string limit is rejected", path: "/v1/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":"64"}`, wantError: true, wantCode: manager.ErrCodeMaxTokensMismatch, wantParam: "max_tokens"},
		{name: "fractional limit is rejected", path: "/v1/completions", headers: []string{"64"},
			body: `{"model":"m","max_tokens":64.0}`, wantError: true, wantCode: manager.ErrCodeMaxTokensMismatch, wantParam: "max_tokens"},

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
				if apiErr.Status != http.StatusBadRequest || apiErr.Code != tt.wantCode || apiErr.Param != tt.wantParam {
					t.Fatalf("error = %+v, want 400 code=%q param=%q", apiErr, tt.wantCode, tt.wantParam)
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
