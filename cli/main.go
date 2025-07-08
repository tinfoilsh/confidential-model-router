package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/tinfoilsh/confidential-inference-proxy/manager"
)

var (
	defaultProxyEndpoint = "http://localhost:8089"
	enclavesPath         = "/.well-known/tinfoil-enclaves"
	apiKey               = os.Getenv("TINFOIL_PROXY_API_KEY")
)

var (
	proxyEndpoint string
	rootCmd       = &cobra.Command{
		Use:   "proxyctl",
		Short: "proxyctl - control tool for confidential inference proxy",
	}
)

func init() {
	rootCmd.PersistentFlags().StringVar(&proxyEndpoint, "endpoint", defaultProxyEndpoint, "Proxy endpoint URL")

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
			url := fmt.Sprintf("%s%s?model=%s&host=%s", proxyEndpoint, enclavesPath, model, host)

			req, err := http.NewRequest(http.MethodPut, url, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to create request: %v\n", err)
				os.Exit(1)
			}

			req.Header.Set("Authorization", "Bearer "+apiKey)

			response, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to send request: %v\n", err)
				os.Exit(1)
			}
			defer response.Body.Close()

			if response.StatusCode != http.StatusOK {
				body, err := io.ReadAll(response.Body)
				if err == nil {
					fmt.Fprintf(os.Stderr, "failed to add model: server returned status %d: %s\n", response.StatusCode, string(body))
				} else {
					fmt.Fprintf(os.Stderr, "failed to add model: server returned status %d\n", response.StatusCode)
				}
				os.Exit(1)
			}

			fmt.Printf("Successfully added model %s with host %s\n", model, host)
		},
	}

	// Update command
	updateCmd := &cobra.Command{
		Use:   "update [model] [host]",
		Short: "Update an existing model's host",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if apiKey == "" {
				fmt.Fprintf(os.Stderr, "TINFOIL_PROXY_API_KEY environment variable is not set\n")
				os.Exit(1)
			}

			url := fmt.Sprintf("%s%s?model=%s", proxyEndpoint, enclavesPath, args[0])
			req, err := http.NewRequest(http.MethodPatch, url, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to create request: %v\n", err)
				os.Exit(1)
			}

			req.Header.Set("Authorization", "Bearer "+apiKey)

			response, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to send request: %v\n", err)
				os.Exit(1)
			}
			defer response.Body.Close()

			if response.StatusCode != http.StatusOK {
				body, err := io.ReadAll(response.Body)
				if err == nil {
					fmt.Fprintf(os.Stderr, "failed to update model: server returned status %d: %s\n", response.StatusCode, string(body))
				} else {
					fmt.Fprintf(os.Stderr, "failed to update model: server returned status %d\n", response.StatusCode)
				}
				os.Exit(1)
			}

			fmt.Printf("Successfully updated model\n")
		},
	}

	// List command
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered models",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			url := proxyEndpoint + enclavesPath
			resp, err := http.Get(url)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to get models: %v\n", err)
				os.Exit(1)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				fmt.Fprintf(os.Stderr, "failed to list models: server returned status %d\n", resp.StatusCode)
				os.Exit(1)
			}

			var response struct {
				Models map[string]*manager.Model `json:"models"`
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to read response body: %v\n", err)
				os.Exit(1)
			}

			if err := json.Unmarshal(body, &response); err != nil {
				fmt.Fprintf(os.Stderr, "failed to parse response: %v\n", err)
				os.Exit(1)
			}

			// Create a new tabwriter for formatted output
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "MODEL\tREPOSITORY\tTAG\tENCLAVES")

			for name, model := range response.Models {
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
			url := fmt.Sprintf("%s%s?model=%s&host=%s", proxyEndpoint, enclavesPath, model, host)

			req, err := http.NewRequest(http.MethodDelete, url, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to create request: %v\n", err)
				os.Exit(1)
			}

			req.Header.Set("Authorization", "Bearer "+apiKey)

			response, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to send request: %v\n", err)
				os.Exit(1)
			}
			defer response.Body.Close()

			if response.StatusCode != http.StatusOK {
				body, err := io.ReadAll(response.Body)
				if err == nil {
					fmt.Fprintf(os.Stderr, "failed to delete enclave: server returned status %d: %s\n", response.StatusCode, string(body))
				} else {
					fmt.Fprintf(os.Stderr, "failed to delete enclave: server returned status %d\n", response.StatusCode)
				}
				os.Exit(1)
			}

			fmt.Printf("Successfully deleted enclave %s from model %s\n", host, model)
		},
	}

	rootCmd.AddCommand(addCmd, updateCmd, listCmd, deleteCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
