package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"

	log "github.com/sirupsen/logrus"
	"github.com/tinfoilsh/tinfoil-go"
)

var (
	port    = flag.String("l", "8089", "port to listen on")
	verbose = flag.Bool("v", false, "enable verbose logging")
)

type enclave struct {
	Host        string               `json:"host"`
	GroundTruth *tinfoil.GroundTruth `json:"ground_truth,omitempty"`
	Error       string               `json:"error,omitempty"`

	proxy *httputil.ReverseProxy `json:"-"`
}

type model struct {
	Repo             string     `json:"repo"`
	Enclaves         []*enclave `json:"enclaves"` // all enclaves
	VerifiedEnclaves []*enclave `json:"-"`        // only verified good enclaves
}

var (
	counter uint64
	models  = make(map[string]*model)
)

func addEnclave(modelName, host, repo string) error {
	if _, ok := models[modelName]; !ok {
		models[modelName] = &model{
			Repo: repo,
		}
	}

	proxy, groundTruth, err := reverseProxy(host, repo)
	e := &enclave{
		Host:        host,
		GroundTruth: groundTruth,
		proxy:       proxy,
	}
	models[modelName].Enclaves = append(models[modelName].Enclaves, e)

	if err != nil {
		log.Errorf("failed to verify enclave %s: %v", host, err)
		e.Error = err.Error()
		return err
	} else {
		models[modelName].VerifiedEnclaves = append(models[modelName].VerifiedEnclaves, e)
	}

	return nil
}

func reverseProxy(host, repo string) (*httputil.ReverseProxy, *tinfoil.GroundTruth, error) {
	client := tinfoil.NewSecureClient(host, repo)
	groundTruth, err := client.Verify()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to verify client: %w", err)
	}

	httpClient, err := client.HTTPClient()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get HTTP client: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "https",
		Host:   host,
	})
	if proxy == nil {
		return nil, nil, fmt.Errorf("failed to create reverse proxy")
	}
	proxy.Transport = httpClient.Transport

	return proxy, groundTruth, nil
}

func main() {
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	config, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	for modelName, modelConfig := range config.Models {
		for _, host := range modelConfig.EnclaveHosts {
			if err := addEnclave(modelName, host, modelConfig.Repo); err != nil {
				log.Warn(err)
			}
		}
	}

	http.HandleFunc("/.well-known/tinfoil-proxy-status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"models": models,
		})
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		modelName := "update-demo"
		if _, ok := models[modelName]; !ok {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}

		count := atomic.AddUint64(&counter, 1)
		enclave := models[modelName].VerifiedEnclaves[(count-1)%uint64(len(models[modelName].VerifiedEnclaves))]
		if enclave == nil {
			http.Error(w, "no enclaves available", http.StatusServiceUnavailable)
			return
		}

		log.Debugf("%s serving request\n", enclave.Host)
		w.Header().Set("X-Tinfoil-Enclave", enclave.Host)
		enclave.proxy.ServeHTTP(w, r)
	})

	log.Printf("Starting proxy server on port %s\n", *port)
	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatal(err)
	}
}
