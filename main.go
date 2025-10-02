package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-inference-proxy/manager"
)

//go:embed config.yml
var configFile []byte // Initial (attested) config

var (
	port            = flag.String("l", "8089", "port to listen on")
	controlPlaneURL = flag.String("C", "https://api.tinfoil.sh", "control plane URL")
	verbose         = flag.Bool("v", false, "enable verbose logging")
	configURL       = flag.String("c", "https://raw.githubusercontent.com/tinfoilsh/confidential-inference-proxy/main/config.yml", "Path to config.yml, only used for enclaves and not measurements")
)

func jsonError(w http.ResponseWriter, message string, code int) {
	if code == http.StatusOK {
		log.Debugf("jsonError: %s", message)
	} else {
		log.Errorf("jsonError: %s", message)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}

func sendJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
	}
}

func main() {
	flag.Parse()

	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	em, err := manager.NewEnclaveManager(configFile, *controlPlaneURL, *configURL)
	if err != nil {
		log.Fatal(err)
	}
	defer em.Shutdown()
	go em.StartWorker()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var modelName string
		if r.URL.Path == "/" {
			http.Redirect(w, r, "https://docs.tinfoil.sh", http.StatusTemporaryRedirect)
			return
		} else if r.URL.Path == "/v1/audio/transcriptions" || strings.HasPrefix(r.URL.Path, "/v1/audio/") {
			modelName = "audio-processing"
		} else if r.URL.Path == "/v1/convert/file" {
			modelName = "doc-upload"
		} else {
			var body map[string]interface{}
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				jsonError(w, fmt.Sprintf("failed to read request body: %v", err), http.StatusBadRequest)
				return
			}
			if err := json.Unmarshal(bodyBytes, &body); err != nil {
				jsonError(w, fmt.Sprintf("failed to find model parameter in request body: %v", err), http.StatusBadRequest)
				return
			}

			// Extract model name
			modelInterface, ok := body["model"]
			if !ok {
				jsonError(w, "model parameter not found in request body", http.StatusBadRequest)
				return
			}
			modelName, ok = modelInterface.(string)
			if !ok {
				jsonError(w, "model parameter must be a string", http.StatusBadRequest)
				return
			}

			// If streaming request, ensure continuous_usage_stats is enabled
			if stream, ok := body["stream"].(bool); ok && stream {
				// Check if client requested usage stats before we modify anything
				clientRequestedUsage := false
				if streamOptions, ok := body["stream_options"].(map[string]interface{}); ok {
					// Check for OpenAI-style include_usage
					if includeUsage, ok := streamOptions["include_usage"].(bool); ok && includeUsage {
						clientRequestedUsage = true
					}
					// Check for vLLM-style continuous_usage_stats
					if continuousUsage, ok := streamOptions["continuous_usage_stats"].(bool); ok && continuousUsage {
						clientRequestedUsage = true
					}
					streamOptions["continuous_usage_stats"] = true
				} else {
					body["stream_options"] = map[string]interface{}{
						"continuous_usage_stats": true,
					}
				}

				// Set internal header to indicate if client requested usage
				// This header will be used by the proxy to decide whether to filter usage-only chunks
				if clientRequestedUsage {
					r.Header.Set("X-Tinfoil-Client-Requested-Usage", "true")
				}

				// Re-encode the modified body
				newBodyBytes, err := json.Marshal(body)
				if err != nil {
					jsonError(w, "failed to process request body", http.StatusInternalServerError)
					return
				}
				bodyBytes = newBodyBytes
				// Update Content-Length header to match new body size
				r.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
				r.ContentLength = int64(len(bodyBytes))
				log.Debugf("Modified streaming request body to include continuous_usage_stats, client requested usage: %v", clientRequestedUsage)
			}

			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		model, found := em.GetModel(modelName)
		if !found {
			jsonError(w, "model not found", http.StatusNotFound)
			return
		}

		enclave := model.NextEnclave()
		if enclave == nil {
			jsonError(w, "no enclaves available", http.StatusServiceUnavailable)
			return
		}

		log.Debugf("%s serving request\n", enclave)

		enclave.ServeHTTP(w, r)
	})

	http.HandleFunc("/.well-known/tinfoil-proxy", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, em.Status())
	})

	// Setup graceful shutdown
	server := &http.Server{
		Addr:         ":" + *port,
		Handler:      nil,             // Use default ServeMux
		ReadTimeout:  5 * time.Minute, // Increased to support large RAG payloads
		WriteTimeout: 0,               // Disabled to support long-running streaming responses
	}

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("Starting proxy server on port %s\n", *port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	log.Info("Shutting down server...")

	// Create shutdown context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown server
	if err := server.Shutdown(ctx); err != nil {
		log.WithError(err).Error("Failed to gracefully shutdown server")
	}

	log.Info("Server stopped")
}
