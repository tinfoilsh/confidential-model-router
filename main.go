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
	"gopkg.in/yaml.v2"

	"github.com/tinfoilsh/confidential-inference-proxy/manager"
	"github.com/tinfoilsh/verifier/github"
)

//go:embed config.yml
var configFile []byte

var (
	extConfigFile = flag.String("e", "/tinfoil/external-config.yml", "path to external config file")
	port          = flag.String("l", "8089", "port to listen on")
	verbose       = flag.Bool("v", false, "enable verbose logging")
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

func loadExternalConfig() (string, string, error) {
	data, err := os.ReadFile(*extConfigFile)
	if err != nil {
		return "", "", err
	}

	var config struct {
		APIKey       string `yaml:"proxy-api-key"`
		ControlPlane string `yaml:"control-plane"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return "", "", err
	}
	return config.APIKey, config.ControlPlane, nil
}

func main() {
	flag.Parse()

	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	apiKey, controlPlaneURL, err := loadExternalConfig()
	if err != nil {
		log.Fatal(err)
	}

	mng, err := manager.NewEnclaveManager(configFile, controlPlaneURL)
	if err != nil {
		log.Fatal(err)
	}

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
				if streamOptions, ok := body["stream_options"].(map[string]interface{}); ok {
					streamOptions["continuous_usage_stats"] = true
				} else {
					body["stream_options"] = map[string]interface{}{
						"continuous_usage_stats": true,
					}
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
				log.Debugf("Modified streaming request body to include continuous_usage_stats")
			}
			
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		model, found := mng.GetModel(modelName)
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
		switch r.Method {
		case http.MethodGet:
			sendJSON(w, map[string]any{
				"models": mng.Models(),
			})
			return
		case http.MethodPut:
			if r.Header.Get("Authorization") != "Bearer "+apiKey {
				jsonError(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			modelName := r.URL.Query().Get("model")
			host := r.URL.Query().Get("host")
			if err := mng.AddEnclave(modelName, host); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}

			m, found := mng.GetModel(modelName)
			if !found {
				jsonError(w, "model not found", http.StatusNotFound)
				return
			}
			sendJSON(w, map[string]any{
				"model": m,
			})
		case http.MethodPatch:
			if r.Header.Get("Authorization") != "Bearer "+apiKey {
				jsonError(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			modelName := r.URL.Query().Get("model")
			model, found := mng.GetModel(modelName)
			if !found {
				jsonError(w, "model not found", http.StatusNotFound)
				return
			}

			// Check if model is up to date
			tag, err := github.FetchLatestTag(model.Repo)
			if err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if model.Tag == tag {
				jsonError(w, "model already up to date", http.StatusBadRequest)
				return
			}

			if err := mng.UpdateModel(modelName); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}

			jsonError(w, fmt.Sprintf("Model %s updated", modelName), http.StatusOK)
			return
		case http.MethodDelete:
			if r.Header.Get("Authorization") != "Bearer "+apiKey {
				jsonError(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			modelName := r.URL.Query().Get("model")
			host := r.URL.Query().Get("host")
			if modelName == "" || host == "" {
				jsonError(w, "model and host parameters are required", http.StatusBadRequest)
				return
			}

			if err := mng.DeleteEnclave(modelName, host); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}

			jsonError(w, fmt.Sprintf("Enclave %s removed from model %s", host, modelName), http.StatusOK)
			return
		default:
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	})

	// Setup graceful shutdown
	server := &http.Server{
		Addr:         ":" + *port,
		Handler:      nil, // Use default ServeMux
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
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

	// Stop billing collector to ensure any remaining events are sent
	mng.StopBillingCollector()

	log.Info("Server stopped")
}
