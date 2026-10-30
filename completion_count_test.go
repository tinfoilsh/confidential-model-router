package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/manager"
)

func TestApplyCompletionCountHeader(t *testing.T) {
	type testCase struct {
		name      string
		path      string
		headers   []string
		body      string
		want      string
		wantError bool
	}
	tests := []testCase{
		{name: "absent leaves body unchanged", body: `{"n":3,"best_of":5}`, want: `{"n":3,"best_of":5}`},
		{name: "adds missing count", headers: []string{"2"}, body: `{"model":"m"}`, want: `{"model":"m","n":2}`},
		{name: "replaces larger count", headers: []string{"1"}, body: `{"n":3}`, want: `{"n":1}`},
		{name: "replaces smaller count", headers: []string{"3"}, body: `{"n":1}`, want: `{"n":3}`},
		{name: "replaces malformed count", headers: []string{"2"}, body: `{"n":"many"}`, want: `{"n":2}`},
		{name: "matches output header parsing", headers: []string{" +02 "}, body: `{}`, want: `{"n":2}`},
		{name: "removes redundant best_of", headers: []string{"3"}, body: `{"best_of":1}`, want: `{"n":3}`},
		{name: "responses ignored", path: "/v1/responses", headers: []string{"bad"}, body: `{"n":3}`, want: `{"n":3}`},
		{name: "legacy completions ignored", path: "/v1/completions", headers: []string{"2"}, body: `{"n":3}`, want: `{"n":3}`},
		{name: "embeddings ignored", path: "/v1/embeddings", headers: []string{"bad"}, body: `{}`, want: `{}`},
	}
	for _, value := range []string{"", " ", "0", "-1", "1.5", "many", "1,2", "9223372036854775808"} {
		tests = append(tests, testCase{name: "invalid header " + value, headers: []string{value}, body: `{}`, wantError: true})
	}
	tests = append(tests, testCase{name: "duplicate header", headers: []string{"1", "1"}, body: `{}`, wantError: true})
	for _, value := range []string{"2", "0", "-1", "1.5", "1.0", `"1"`, "null", "true", "{}", "[]", "9223372036854775808"} {
		tests = append(tests, testCase{name: "invalid best_of " + value, headers: []string{"1"}, body: `{"best_of":` + value + `}`, wantError: true})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path
			if path == "" {
				path = "/v1/chat/completions"
			}
			h := http.Header{}
			for _, v := range tt.headers {
				h.Add(manager.CompletionCountRequestHeader, v)
			}
			body, err := decodeJSONBody([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			apiErr := applyCompletionCountHeader(h, path, body)
			if tt.wantError {
				if apiErr == nil || apiErr.Status != http.StatusBadRequest || apiErr.Type != manager.ErrTypeInvalidRequest {
					t.Fatalf("want invalid-request 400, got %+v", apiErr)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("unexpected error: %+v", apiErr)
			}
			got, _ := json.Marshal(body)
			wantBody, err := decodeJSONBody([]byte(tt.want))
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(wantBody)
			if string(got) != string(want) {
				t.Fatalf("body=%s, want %s", got, want)
			}
		})
	}
}
