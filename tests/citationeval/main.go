// Verify that gpt-oss outputs Harmony prompts by default

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

var (
	baseURL    = flag.String("base-url", "http://localhost:8089/v1", "router base URL")
	modelName  = flag.String("model", "gpt-oss-120b", "model identifier")
	numQueries = flag.Int("queries", 5, "number of eval queries to run")
	rawOutput  = flag.Bool("raw", false, "print full raw response JSON")
)

// Queries designed to trigger web search and citations.
var evalQueries = []string{
	"What are the latest headlines today?",
	"Who won the most recent NBA game?",
	"What is the current price of Bitcoin?",
	"What happened at the OpenAI dev day?",
	"Summarize the top 3 results for 'climate change 2026'",
}

// Citation pattern definitions.
type patternDef struct {
	Name    string
	Re      *regexp.Regexp
	Example string
}

var patterns = []patternDef{
	{
		Name: "Harmony 【N†LX】",
		Re:   regexp.MustCompile(`\x{3010}\d+\x{2020}L\d+(-L\d+)?\x{3011}`),
	},
	{
		Name: "Markdown [label](url)",
		Re:   regexp.MustCompile(`\[[^\]]+\]\(https?://[^\s)]+\)`),
	},
	{
		Name: "Other 【...】",
		Re:   regexp.MustCompile(`\x{3010}[^\x{3011}]+\x{3011}`),
	},
}

// patternResult tracks matches for a single pattern across all queries.
type patternResult struct {
	Count   int
	Example string
}

func main() {
	flag.Parse()

	apiKey := mustEnv("OPENAI_API_KEY")

	httpClient := &http.Client{Timeout: 5 * time.Minute}
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(*baseURL),
		option.WithHTTPClient(httpClient),
	)

	queries := evalQueries
	if *numQueries < len(queries) {
		queries = queries[:*numQueries]
	}

	// Aggregate results across all queries.
	aggregate := make([]patternResult, len(patterns))

	fmt.Printf("Citation Eval Tool\n")
	fmt.Printf("==================\n")
	fmt.Printf("Endpoint: %s\n", *baseURL)
	fmt.Printf("Model:    %s\n", *modelName)
	fmt.Printf("Queries:  %d\n\n", len(queries))

	for i, query := range queries {
		fmt.Printf("─── Query %d/%d ───\n", i+1, len(queries))
		fmt.Printf("Q: %s\n\n", query)

		content, rawJSON, err := runQuery(context.Background(), client, query)
		if err != nil {
			fmt.Printf("ERROR: %v\n\n", err)
			continue
		}

		if *rawOutput {
			fmt.Printf("Raw JSON:\n%s\n\n", rawJSON)
		}

		fmt.Printf("Response:\n%s\n\n", content)

		// Scan for citation patterns.
		fmt.Printf("Patterns found:\n")
		found := false
		for j, p := range patterns {
			matches := p.Re.FindAllString(content, -1)
			if len(matches) > 0 {
				found = true
				fmt.Printf("  %-30s  %d match(es)  e.g. %s\n", p.Name, len(matches), truncate(matches[0], 80))
				aggregate[j].Count += len(matches)
				if aggregate[j].Example == "" {
					aggregate[j].Example = matches[0]
				}
			}
		}
		if !found {
			fmt.Printf("  (none)\n")
		}
		fmt.Println()
	}

	// Print aggregate summary table.
	fmt.Printf("═══ Aggregate Summary ═══\n\n")
	fmt.Printf("%-30s | %5s | %s\n", "Pattern", "Count", "Example")
	fmt.Printf("%s-|-%s-|-%s\n", strings.Repeat("-", 30), strings.Repeat("-", 5), strings.Repeat("-", 40))
	for j, p := range patterns {
		example := aggregate[j].Example
		if example == "" {
			example = "-"
		}
		fmt.Printf("%-30s | %5d | %s\n", p.Name, aggregate[j].Count, truncate(example, 60))
	}
	fmt.Println()
}

func runQuery(ctx context.Context, client openai.Client, query string) (content string, rawJSON string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(*modelName),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(query),
		},
		WebSearchOptions: openai.ChatCompletionNewParamsWebSearchOptions{
			SearchContextSize: "medium",
		},
	}

	completion, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		return "", "", fmt.Errorf("chat completion failed: %w", err)
	}

	// Extract raw JSON for -raw flag.
	raw, _ := json.MarshalIndent(completion, "", "  ")
	rawJSON = string(raw)

	// Extract text content from the response.
	if len(completion.Choices) == 0 {
		return "", rawJSON, fmt.Errorf("no choices in response")
	}

	return completion.Choices[0].Message.Content, rawJSON, nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func mustEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("missing %s environment variable", key)
	}
	return value
}
