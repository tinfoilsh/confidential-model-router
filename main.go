package main

import (
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

type Enclave struct {
	Repo string
	Host string

	proxy *httputil.ReverseProxy
}

var (
	counter uint64

	models = map[string][]*Enclave{
		"qwen2-5-72b": {
			{
				Repo: "tinfoilsh/confidential-qwen2-5-72b",
				Host: "qwen2-5-72b.model.tinfoil.sh",
			}, {
				Repo: "tinfoilsh/confidential-qwen2-5-72b-2",
				Host: "qwen2-5-72b-2.model.tinfoil.sh",
			},
		},
	}
)

func reverseProxy(host, repo string) (*httputil.ReverseProxy, error) {
	client := tinfoil.NewSecureClient(host, repo)
	_, err := client.Verify()
	if err != nil {
		return nil, fmt.Errorf("failed to verify client: %w", err)
	}

	httpClient, err := client.HTTPClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get HTTP client: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: "https",
		Host:   host,
	})
	if proxy == nil {
		return nil, fmt.Errorf("failed to create reverse proxy")
	}
	proxy.Transport = httpClient.Transport

	return proxy, nil
}

func main() {
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	for model, enclaves := range models {
		for _, enclave := range enclaves {
			log.Printf("[%s] Verifying %s\n", model, enclave.Host)

			proxy, err := reverseProxy(enclave.Host, enclave.Repo)
			if err != nil {
				log.Fatal(err)
			}
			enclave.proxy = proxy
		}
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		enclaves := models["qwen2-5-72b"]
		count := atomic.AddUint64(&counter, 1)
		index := (count - 1) % uint64(len(enclaves))
		proxy := enclaves[index].proxy

		log.Debugf("[%s] Serving request %s\n", enclaves[index].Host, r.URL.Path)

		w.Header().Set("X-Tinfoil-Enclave", enclaves[index].Host)

		proxy.ServeHTTP(w, r)
	})

	log.Printf("Starting proxy server on port %s\n", *port)
	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatal(err)
	}
}
