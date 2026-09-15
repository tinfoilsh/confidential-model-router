package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListModelsReadsPerEndpointVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != enclavesPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"models":{"model":{"repo":"org/model","enclaves":{"older":{"tag":"v1","measurement":{"type":"example"}},"newer":{"tag":"v2"}}}},"errors":[]}`)
	}))
	defer server.Close()
	previous := proxyEndpoint
	proxyEndpoint = server.URL
	t.Cleanup(func() { proxyEndpoint = previous })
	response, err := listModels()
	if err != nil {
		t.Fatal(err)
	}
	model := response.Models["model"]
	if model.Repo != "org/model" || model.Enclaves["older"].Tag != "v1" || model.Enclaves["newer"].Tag != "v2" {
		t.Fatal("per-endpoint versions were not decoded")
	}
}
