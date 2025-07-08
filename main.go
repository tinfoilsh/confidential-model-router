package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"

	"github.com/tinfoilsh/confidential-inference-proxy/manager"
	"github.com/tinfoilsh/verifier/github"
)

//go:embed config.yml
var configFile []byte

var (
	extConfigFile = flag.String("e", "/mnt/ramdisk/external-config.yml", "path to external config file")
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

func loadAPIKey() (string, error) {
	apiKey, err := os.ReadFile(*extConfigFile)
	if err != nil {
		return "", err
	}

	var config struct {
		APIKey string `yaml:"proxy-api-key"`
	}
	if err := yaml.Unmarshal(apiKey, &config); err != nil {
		return "", err
	}
	return config.APIKey, nil
}

func main() {
	flag.Parse()

	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	apiKey, err := loadAPIKey()
	if err != nil {
		log.Fatal(err)
	}

	mng, err := manager.NewEnclaveManager(configFile)
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			jsonError(w, fmt.Sprintf("failed to read request body: %v", err), http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			jsonError(w, fmt.Sprintf("failed to find model parameter in request body: %v", err), http.StatusBadRequest)
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		model, found := mng.GetModel(body.Model)
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

	http.HandleFunc("/.well-known/tinfoil-enclaves", func(w http.ResponseWriter, r *http.Request) {
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

	log.Printf("Starting proxy server on port %s\n", *port)
	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatal(err)
	}
}
