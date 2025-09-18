package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/tinfoilsh/confidential-inference-proxy/manager"
	"gopkg.in/yaml.v2"
)

var (
	defaultProxyEndpoint = "https://inference.tinfoil.sh"
	enclavesPath         = "/.well-known/tinfoil-proxy"
	apiKey               = os.Getenv("TINFOIL_PROXY_API_KEY")
	verbose              bool
)

var (
	proxyEndpoint string
	rootCmd       = &cobra.Command{
		Use:   "proxyctl",
		Short: "proxyctl - control tool for confidential inference proxy",
	}
)

func listModels() (map[string]*manager.Model, error) {
	url := proxyEndpoint + enclavesPath
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list models: server returned status %d", resp.StatusCode)
	}

	var response struct {
		Models map[string]*manager.Model `json:"models"`
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}

	return response.Models, nil
}

func update(model string) error {
	url := fmt.Sprintf("%s%s?model=%s", proxyEndpoint, enclavesPath, model)
	req, err := http.NewRequest(http.MethodPatch, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		var resp struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(response.Body).Decode(&resp); err == nil {
			return fmt.Errorf("failed to update %s: %s", model, resp.Error)
		} else {
			return fmt.Errorf("failed to update %s: server returned status %d", model, response.StatusCode)
		}
	}

	return nil
}

func removeEnclave(model, host string) error {
	url := fmt.Sprintf("%s%s?model=%s&host=%s", proxyEndpoint, enclavesPath, model, host)

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, err := io.ReadAll(response.Body)
		if err == nil {
			return fmt.Errorf("failed to delete enclave: server returned status %d: %s", response.StatusCode, string(body))
		} else {
			return fmt.Errorf("failed to delete enclave: server returned status %d", response.StatusCode)
		}
	}

	return nil
}

func addEnclave(model, host string) error {
	url := fmt.Sprintf("%s%s?model=%s&host=%s", proxyEndpoint, enclavesPath, model, host)

	req, err := http.NewRequest(http.MethodPut, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, err := io.ReadAll(response.Body)
		if err == nil {
			return fmt.Errorf("failed to add model: server returned status %d: %s", response.StatusCode, string(body))
		} else {
			return fmt.Errorf("failed to add model: server returned status %d", response.StatusCode)
		}
	}

	return nil
}

func reloadEnclave(model, host string) error {
	if err := removeEnclave(model, host); err != nil {
		return fmt.Errorf("failed to remove enclave %s from model %s: %v", host, model, err)
	}
	if err := addEnclave(model, host); err != nil {
		return fmt.Errorf("failed to add enclave %s to model %s: %v", host, model, err)
	}
	return nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&proxyEndpoint, "endpoint", defaultProxyEndpoint, "Proxy endpoint URL")
	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "Verbose output")

	// Add command
	addCmd := &cobra.Command{
		Use:   "add [model] [host]",
		Short: "Add a new model with host",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if apiKey == "" {
				fmt.Fprintf(os.Stderr, "TINFOIL_PROXY_API_KEY environment variable is not set\n")
				os.Exit(1)
			}

			model, host := args[0], args[1]
			if err := addEnclave(model, host); err != nil {
				fmt.Fprintf(os.Stderr, "failed to add %s: %v\n", host, err)
				os.Exit(1)
			}

			fmt.Printf("Successfully added model %s with host %s\n", model, host)
		},
	}

	// Update command
	updateCmd := &cobra.Command{
		Use:   "update [model] [host]",
		Short: "Update an existing model's host",
		Run: func(cmd *cobra.Command, args []string) {
			if apiKey == "" {
				fmt.Fprintf(os.Stderr, "TINFOIL_PROXY_API_KEY environment variable is not set\n")
				os.Exit(1)
			}

			if len(args) == 0 {
				models, err := listModels()
				if err != nil {
					fmt.Fprintf(os.Stderr, "failed to list models: %v\n", err)
					os.Exit(1)
				}
				for modelName := range models {
					if err := update(modelName); err != nil {
						fmt.Fprintf(os.Stderr, "failed to update %s: %v\n", modelName, err)
					} else {
						fmt.Printf("Successfully updated model %s\n", modelName)
					}
				}
			} else if len(args) == 1 {
				if err := update(args[0]); err != nil {
					fmt.Fprintf(os.Stderr, "failed to update %s: %v\n", args[0], err)
					os.Exit(1)
				}
			} else {
				fmt.Fprintf(os.Stderr, "Usage: proxyctl update [model] [host]\n")
				os.Exit(1)
			}
		},
	}

	// List command
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered models",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			models, err := listModels()
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to list models: %v\n", err)
				os.Exit(1)
			}

			// Create a new tabwriter for formatted output
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "MODEL\tREPOSITORY\tTAG\tENCLAVES")

			for name, model := range models {
				enclaves := "-"
				if len(model.Enclaves) > 0 {
					enclaves = ""
					for _, e := range model.Enclaves {
						enclaves += e.String() + ", "
					}
					enclaves = enclaves[:len(enclaves)-2]
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					name,
					model.Repo,
					model.Tag,
					enclaves,
				)
			}
			w.Flush()
		},
	}

	// Delete command
	deleteCmd := &cobra.Command{
		Use:   "delete [model] [host]",
		Short: "Delete an enclave from a model",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if apiKey == "" {
				fmt.Fprintf(os.Stderr, "TINFOIL_PROXY_API_KEY environment variable is not set\n")
				os.Exit(1)
			}

			model, host := args[0], args[1]
			if err := removeEnclave(model, host); err != nil {
				fmt.Fprintf(os.Stderr, "failed to remove enclave %s from model %s: %v\n", host, model, err)
				os.Exit(1)
			}

			fmt.Printf("Successfully deleted enclave %s from model %s\n", host, model)
		},
	}

	// Recreate command
	recreateCmd := &cobra.Command{
		Use:   "recreate [model] [host]",
		Short: "Recreate an enclave for a model",
		Run: func(cmd *cobra.Command, args []string) {
			if apiKey == "" {
				fmt.Fprintf(os.Stderr, "TINFOIL_PROXY_API_KEY environment variable is not set\n")
				os.Exit(1)
			}

			if len(args) == 2 {
				model, host := args[0], args[1]
				if err := reloadEnclave(model, host); err != nil {
					fmt.Fprintf(os.Stderr, "failed to recreate enclave %s from model %s: %v\n", host, model, err)
					os.Exit(1)
				}
				fmt.Printf("Successfully recreated enclave %s from model %s\n", host, model)
			} else if len(args) == 1 {
				models, err := listModels()
				if err != nil {
					fmt.Fprintf(os.Stderr, "failed to list models: %v\n", err)
					os.Exit(1)
				}
				modelConfig, ok := models[args[0]]
				if !ok {
					fmt.Fprintf(os.Stderr, "model %s not found\n", args[0])
					os.Exit(1)
				}

				for _, enclave := range modelConfig.Enclaves {
					if err := reloadEnclave(args[0], enclave.String()); err != nil {
						fmt.Fprintf(os.Stderr, "failed to recreate enclave %s from model %s: %v\n", enclave.String(), args[0], err)
						os.Exit(1)
					}
					fmt.Printf("Successfully recreated enclave %s from model %s\n", enclave.String(), args[0])
				}
			} else {
				fmt.Fprintf(os.Stderr, "Usage: proxyctl recreate [model] [host]\n")
				os.Exit(1)
			}
		},
	}

	// Apply command
	applyCmd := &cobra.Command{
		Use:   "apply [runtime.yml]",
		Short: "Apply runtime configuration from YAML file",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if apiKey == "" {
				fmt.Fprintf(os.Stderr, "TINFOIL_PROXY_API_KEY environment variable is not set\n")
				os.Exit(1)
			}

			// Read and parse runtime.yml
			configPath := args[0]
			if !filepath.IsAbs(configPath) {
				wd, err := os.Getwd()
				if err != nil {
					fmt.Fprintf(os.Stderr, "failed to get working directory: %v\n", err)
					os.Exit(1)
				}
				configPath = filepath.Join(wd, configPath)
			}

			configData, err := os.ReadFile(configPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to read config file: %v\n", err)
				os.Exit(1)
			}

			var runtimeConfig struct {
				Models map[string][]string `yaml:"models"` // model name -> list of enclave hosts
			}
			if err := yaml.Unmarshal(configData, &runtimeConfig); err != nil {
				fmt.Fprintf(os.Stderr, "failed to parse config file: %v\n", err)
				os.Exit(1)
			}

			totalAdded := 0
			totalRemoved := 0

			// Get current state
			url := proxyEndpoint + enclavesPath
			resp, err := http.Get(url)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get current state: %v\n", err)
				os.Exit(1)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				fmt.Fprintf(os.Stderr, "failed to get current state: server returned status %d\n", resp.StatusCode)
				os.Exit(1)
			}

			var currentState struct {
				Models map[string]*manager.Model `json:"models"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&currentState); err != nil {
				fmt.Fprintf(os.Stderr, "failed to parse current state: %v\n", err)
				os.Exit(1)
			}

			// Process each model in the runtime config
			for modelName, desiredHosts := range runtimeConfig.Models {
				// Skip if model doesn't exist in current state
				currentModel, exists := currentState.Models[modelName]
				if !exists {
					fmt.Printf("Skipping model %s: not found in current state\n", modelName)
					continue
				}

				// Get current hosts
				currentHosts := make(map[string]bool)
				for _, enclave := range currentModel.Enclaves {
					currentHosts[enclave.String()] = true
				}

				// Add missing hosts
				added := 0
				for _, host := range desiredHosts {
					if !currentHosts[host] {
						log.Debugf("Adding enclave %s to model %s\n", host, modelName)
						url := fmt.Sprintf("%s%s?model=%s&host=%s", proxyEndpoint, enclavesPath, modelName, host)
						req, err := http.NewRequest(http.MethodPut, url, nil)
						if err != nil {
							fmt.Fprintf(os.Stderr, "failed to create request: %v\n", err)
							continue
						}
						req.Header.Set("Authorization", "Bearer "+apiKey)
						response, err := http.DefaultClient.Do(req)
						if err != nil {
							fmt.Fprintf(os.Stderr, "failed to add enclave %s to model %s: %v\n", host, modelName, err)
							continue
						}
						defer response.Body.Close()

						if response.StatusCode != http.StatusOK {
							bodyBytes, err := io.ReadAll(response.Body)
							if err != nil {
								fmt.Fprintf(os.Stderr, "failed to read response body: %v\n", err)
								continue
							}
							body := strings.TrimSpace(string(bodyBytes))
							fmt.Fprintf(os.Stderr, "failed to add enclave %s to model %s: %d %s\n", host, modelName, response.StatusCode, body)
							continue
						}

						added++
					} else {
						log.Debugf("Enclave %s already exists for model %s\n", host, modelName)
					}
				}

				// Remove extra hosts
				desiredHostsMap := make(map[string]bool)
				for _, host := range desiredHosts {
					desiredHostsMap[host] = true
				}
				removed := 0
				for _, enclave := range currentModel.Enclaves {
					if !desiredHostsMap[enclave.String()] {
						removed++
						log.Debugf("Removing enclave %s from model %s\n", enclave, modelName)
						url := fmt.Sprintf("%s%s?model=%s&host=%s", proxyEndpoint, enclavesPath, modelName, enclave)
						req, err := http.NewRequest(http.MethodDelete, url, nil)
						if err != nil {
							fmt.Fprintf(os.Stderr, "failed to create request: %v\n", err)
							continue
						}
						req.Header.Set("Authorization", "Bearer "+apiKey)
						response, err := http.DefaultClient.Do(req)
						if err != nil {
							fmt.Fprintf(os.Stderr, "failed to remove enclave %s: %v\n", enclave, err)
							continue
						}
						response.Body.Close()
						if response.StatusCode != http.StatusOK {
							fmt.Fprintf(os.Stderr, "failed to remove enclave %s: server returned status %d\n", enclave, response.StatusCode)
							continue
						}
					}
				}

				fmt.Printf("Added %d enclaves and removed %d enclaves for model %s\n", added, removed, modelName)
				totalAdded += added
				totalRemoved += removed
			}

			fmt.Printf("Applied runtime configuration (+%d -%d)\n", totalAdded, totalRemoved)
		},
	}

	rootCmd.AddCommand(addCmd, updateCmd, listCmd, deleteCmd, applyCmd, recreateCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
