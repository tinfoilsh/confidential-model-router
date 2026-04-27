// Verify what citation format gpt-oss produces under different prompting strategies.

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
	numQueries = flag.Int("queries", 4, "number of eval queries to run (max 4)")
	rawOutput  = flag.Bool("raw", false, "print full raw response JSON")
)

// Queries designed to trigger web search and citations.
var evalQueries = []string{
	"What are the latest headlines today?",
	"Who won the most recent NBA game?",
	"What is the current price of Bitcoin?",
	"Summarize the top 3 results for 'climate change 2026'",
}

// promptMode defines a system prompt strategy to test.
type promptMode struct {
	Name   string
	System string // empty means no system message
}

var promptModes = []promptMode{
	{
		Name:   "none",
		System: "",
	},
	{
		Name: "cite",
		System: "When responding, cite your sources inline. Reference 1-2 sources per claim. " +
			"Copy URLs character-for-character from the tool output. " +
			"Never invent or paraphrase URLs.",
	},
	{
		Name: "cite-markdown",
		System: "Attach sources by embedding a clickable markdown link to the original URL directly after the sentence it supports. " +
			"Format every citation exactly like this example, copying the punctuation characters verbatim: " +
			"The sky is blue [Example page](https://example.com/article). " +
			"The opening bracket is ASCII 0x5B, the closing bracket is ASCII 0x5D, and the URL is wrapped in ASCII parentheses 0x28 and 0x29. " +
			"Reference 1-2 sources per claim; do not reference every source on every sentence. " +
			"Copy the URL character-for-character from the tool output: preserve or omit a trailing slash exactly as the tool emitted it, " +
			"keep query parameters verbatim, and do not append punctuation, whitespace, zero-width characters, or any other character after the URL before the closing parenthesis. " +
			"Never invent URLs, never paraphrase URLs, and never wrap the link in any other brackets, braces, or quotation marks.",
	},
}

// Citation pattern definitions.
type patternDef struct {
	Name string
	Re   *regexp.Regexp
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

// cell holds results for one (prompt, query) combination.
type cell struct {
	Prompt  string
	Query   string
	Content string
	RawJSON string
	Err     error
	Counts  []int // one per pattern
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

	fmt.Printf("Citation Eval Tool\n")
	fmt.Printf("==================\n")
	fmt.Printf("Endpoint: %s\n", *baseURL)
	fmt.Printf("Model:    %s\n", *modelName)
	fmt.Printf("Queries:  %d\n", len(queries))
	fmt.Printf("Prompts:  %d\n\n", len(promptModes))

	var cells []cell

	for _, mode := range promptModes {
		for _, query := range queries {
			fmt.Printf("─── prompt=%s | query=%q ───\n", mode.Name, truncate(query, 50))

			content, rawJSON, err := runQuery(context.Background(), client, mode.System, query)
			c := cell{
				Prompt:  mode.Name,
				Query:   query,
				Content: content,
				RawJSON: rawJSON,
				Err:     err,
				Counts:  make([]int, len(patterns)),
			}

			if err != nil {
				fmt.Printf("ERROR: %v\n\n", err)
				cells = append(cells, c)
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
				c.Counts[j] = len(matches)
				if len(matches) > 0 {
					found = true
					fmt.Printf("  %-30s  %d match(es)  e.g. %s\n", p.Name, len(matches), truncate(matches[0], 80))
				}
			}
			if !found {
				fmt.Printf("  (none)\n")
			}
			fmt.Println()

			cells = append(cells, c)
		}
	}

	// Print aggregate matrix: rows=patterns, columns=prompt modes.
	fmt.Printf("═══ Aggregate Matrix ═══\n\n")

	// Header row.
	fmt.Printf("%-30s", "Pattern")
	for _, mode := range promptModes {
		fmt.Printf(" | %10s", mode.Name)
	}
	fmt.Println()
	fmt.Printf("%s", strings.Repeat("-", 30))
	for range promptModes {
		fmt.Printf("-|-%s", strings.Repeat("-", 10))
	}
	fmt.Println()

	// One row per pattern, summing counts across queries for each prompt mode.
	for j, p := range patterns {
		fmt.Printf("%-30s", p.Name)
		for _, mode := range promptModes {
			total := 0
			for _, c := range cells {
				if c.Prompt == mode.Name {
					total += c.Counts[j]
				}
			}
			fmt.Printf(" | %10d", total)
		}
		fmt.Println()
	}
	fmt.Println()

	// Per-prompt breakdown with first examples.
	fmt.Printf("═══ Per-Prompt Detail ═══\n\n")
	for _, mode := range promptModes {
		fmt.Printf("── %s ──\n", mode.Name)
		for _, p := range patterns {
			total := 0
			var example string
			for _, c := range cells {
				if c.Prompt != mode.Name {
					continue
				}
				matches := p.Re.FindAllString(c.Content, -1)
				total += len(matches)
				if example == "" && len(matches) > 0 {
					example = matches[0]
				}
			}
			if example == "" {
				example = "-"
			}
			fmt.Printf("  %-28s  %3d  e.g. %s\n", p.Name, total, truncate(example, 60))
		}
		fmt.Println()
	}
}

func runQuery(ctx context.Context, client openai.Client, systemPrompt, query string) (content string, rawJSON string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	var messages []openai.ChatCompletionMessageParamUnion
	if systemPrompt != "" {
		messages = append(messages, openai.SystemMessage(systemPrompt))
	}
	messages = append(messages, openai.UserMessage(query))

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(*modelName),
		Messages: messages,
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
