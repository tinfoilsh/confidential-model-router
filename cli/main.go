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
	defaultProxyEndpoint = "https://inference.tinfoil.sh"
	enclavesPath         = "/.well-known/tinfoil-proxy"
	verbose              bool
)

var (
	proxyEndpoint string
	rootCmd       = &cobra.Command{
		Use:   "proxyctl",
		Short: "proxyctl - confidential inference proxy",
	}
)

type ProxyResponse struct {
	Models map[string]*manager.Model `json:"models"`
	Errors []string                  `json:"errors"`
}

func listModels() (*ProxyResponse, error) {
	url := proxyEndpoint + enclavesPath
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get models: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list models: server returned status %d", resp.StatusCode)
	}

	var response ProxyResponse

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}

	return &response, nil
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&proxyEndpoint, "endpoint", "e", defaultProxyEndpoint, "Proxy endpoint URL")
	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "Verbose output")

	// List command
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered models",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			response, err := listModels()
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to list models: %v\n", err)
				os.Exit(1)
			}

			// Display errors if any
			if len(response.Errors) > 0 {
				fmt.Fprintf(os.Stderr, "ERRORS:\n")
				for _, errMsg := range response.Errors {
					fmt.Fprintf(os.Stderr, "  - %s\n", errMsg)
				}
				fmt.Fprintf(os.Stderr, "\n")
			}

			// Create a new tabwriter for formatted output
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "MODEL\tREPOSITORY\tTAG\tENCLAVES")

			for name, model := range response.Models {
				enclaves := "-"
				if len(model.Enclaves) > 0 {
					enclaveHosts := make([]string, 0, len(model.Enclaves))
					for host := range model.Enclaves {
						enclaveHosts = append(enclaveHosts, host)
					}
					if len(enclaveHosts) > 0 {
						enclaves = ""
						for i, host := range enclaveHosts {
							if i > 0 {
								enclaves += ", "
							}
							enclaves += host
						}
					}
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

	rootCmd.AddCommand(listCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
