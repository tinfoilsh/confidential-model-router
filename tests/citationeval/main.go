// Verify what citation format gpt-oss produces under different prompting strategies.
//
// We construct prefilled conversations: user question → assistant tool_call → tool result with
// fake search snippets → then let the model generate the final response. This hits
// the raw vLLM endpoint directly and shows how the model cites sources natively.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	tinfoil "github.com/tinfoilsh/tinfoil-go/verifier/client"
)

var (
	enclaveHost = flag.String("enclave", "gpt-oss-120b-0.inf6.tinfoil.sh", "enclave hostname")
	repoName    = flag.String("repo", "tinfoilsh/confidential-gpt-oss-120b", "GitHub repo for attestation")
	modelName   = flag.String("model", "gpt-oss-120b", "model identifier")
	numQueries  = flag.Int("queries", 0, "number of eval queries to run (0 = all)")
	rawOutput   = flag.Bool("raw", false, "print full raw response JSON")
)

// searchResult is a single result from a search tool call.
type searchResult struct {
	Title   string
	URL     string
	Content string // may be multi-line
}

// scenario is a user query paired with fake search results.
type scenario struct {
	Query       string
	SearchQuery string
	Results     []searchResult
}

var scenarios = []scenario{
	{
		Query:       "What are the latest headlines today?",
		SearchQuery: "latest headlines today",
		Results: []searchResult{
			{
				Title:   "Global Climate Summit Reaches Historic Agreement",
				URL:     "https://www.reuters.com/world/climate-summit-agreement-2026",
				Content: "World leaders at the 2026 Global Climate Summit in Berlin reached a historic agreement to cut emissions by 50% by 2035, with binding commitments from all major economies.",
			},
			{
				Title:   "Tech Giants Report Record Q1 Earnings",
				URL:     "https://www.bloomberg.com/news/tech-q1-earnings-2026",
				Content: "Major technology companies including Apple, Microsoft, and Alphabet reported better-than-expected first quarter results, driven by AI infrastructure spending.",
			},
			{
				Title:   "NBA Playoffs: Celtics Advance to Conference Finals",
				URL:     "https://www.espn.com/nba/story/celtics-advance-2026",
				Content: "The Boston Celtics defeated the Philadelphia 76ers 112-98 in Game 6 to advance to the Eastern Conference Finals.",
			},
		},
	},
	{
		Query:       "Who won the most recent NBA game?",
		SearchQuery: "most recent NBA game results",
		Results: []searchResult{
			{
				Title:   "Celtics 112, 76ers 98 - Game 6 Recap",
				URL:     "https://www.espn.com/nba/recap/_/gameId/401656789",
				Content: "Jayson Tatum scored 34 points and grabbed 11 rebounds as the Boston Celtics eliminated the Philadelphia 76ers with a 112-98 victory in Game 6 of their Eastern Conference semifinal series.",
			},
			{
				Title:   "NBA Playoffs Bracket 2026",
				URL:     "https://www.nba.com/playoffs/2026/bracket",
				Content: "Full playoff bracket and results. Eastern Conference: Celtics vs 76ers (Celtics win 4-2). Western Conference: Thunder vs Nuggets (series tied 3-3).",
			},
			{
				Title:   "Thunder-Nuggets Game 6 Preview",
				URL:     "https://www.espn.com/nba/preview/_/gameId/401656790",
				Content: "The Oklahoma City Thunder host the Denver Nuggets in a decisive Game 6, with the winner advancing to the Western Conference Finals.",
			},
		},
	},
	{
		Query:       "What is the current price of Bitcoin?",
		SearchQuery: "current bitcoin price",
		Results: []searchResult{
			{
				Title:   "Bitcoin Price Today",
				URL:     "https://www.coindesk.com/price/bitcoin",
				Content: "Bitcoin (BTC) is currently trading at $94,521.30, up 2.3% in the last 24 hours. Market cap: $1.87 trillion. 24h volume: $28.4 billion.",
			},
			{
				Title:   "Bitcoin Surges Past $94K on ETF Inflows",
				URL:     "https://www.coindesk.com/markets/bitcoin-etf-inflows-2026",
				Content: "Bitcoin climbed above $94,000 on Monday as spot Bitcoin ETFs recorded $1.2 billion in net inflows last week, the highest weekly total since January.",
			},
			{
				Title:   "Crypto Market Overview",
				URL:     "https://www.coingecko.com/en/coins/bitcoin",
				Content: "Bitcoin price: $94,521.30 USD. 24h change: +2.3%. 7d change: +8.1%. All-time high: $109,114 (Jan 2025).",
			},
		},
	},
	{
		Query:       "Summarize the top 3 results for 'climate change 2026'",
		SearchQuery: "climate change 2026",
		Results: []searchResult{
			{
				Title:   "2026 Global Climate Report: Temperatures Hit New Record",
				URL:     "https://www.nature.com/articles/climate-report-2026",
				Content: "The World Meteorological Organization confirmed that 2025 was the hottest year on record, with global mean temperature 1.55°C above pre-industrial levels.\nThe report warns that without immediate action, 2°C will be breached by 2040.",
			},
			{
				Title:   "Climate Change 2026: Policy Progress and Gaps",
				URL:     "https://www.unep.org/resources/climate-change-2026-policy",
				Content: "A comprehensive UN Environment Programme report finds that current national pledges would reduce emissions by only 15% by 2030, far short of the 43% needed.\nHowever, renewable energy deployment accelerated by 35% in 2025.",
			},
			{
				Title:   "How Climate Change Is Reshaping Agriculture in 2026",
				URL:     "https://www.bbc.com/news/science-environment-climate-agriculture-2026",
				Content: "Extreme weather events in 2025 caused $180 billion in agricultural losses worldwide.\nFarmers in Southeast Asia and sub-Saharan Africa were disproportionately affected, with crop yields falling 20-30% in some regions.",
			},
		},
	},
}

// formatStandard renders results the way the router does in standard mode.
//
//	Source: <title>
//	URL: <url>
//	<content>
func formatStandard(results []searchResult) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Source: %s\n", r.Title)
		fmt.Fprintf(&b, "URL: %s\n", r.URL)
		b.WriteString(r.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// formatHarmony renders results the way the router does in harmony mode,
// with numbered cursors and per-line labels.
//
//	[1] <title>
//	URL: <url>
//	L1: <line 1>
//	L2: <line 2>
func formatHarmony(results []searchResult) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%d] %s\n", i+1, r.Title)
		fmt.Fprintf(&b, "URL: %s\n", r.URL)
		for j, line := range strings.Split(r.Content, "\n") {
			fmt.Fprintf(&b, "L%d: %s\n", j+1, line)
		}
	}
	return b.String()
}

// resultFormat pairs a name with a formatter.
type resultFormat struct {
	Name   string
	Format func([]searchResult) string
}

var resultFormats = []resultFormat{
	{Name: "standard", Format: formatStandard},
	{Name: "harmony", Format: formatHarmony},
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

// cell holds results for one (format, prompt, query) combination.
type cell struct {
	Format  string
	Prompt  string
	Query   string
	Content string
	RawJSON string
	Err     error
	Counts  []int // one per pattern
}

func main() {
	flag.Parse()

	apiKey := mustEnv("TINFOIL_API_KEY")

	// Set up log file in logs/ directory.
	logDir := filepath.Join(".", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		log.Fatalf("create logs dir: %v", err)
	}
	logName := fmt.Sprintf("citationeval-%s.md", time.Now().Format("2006-01-02T15-04-05"))
	logPath := filepath.Join(logDir, logName)
	logFile, err := os.Create(logPath)
	if err != nil {
		log.Fatalf("create log file: %v", err)
	}
	defer logFile.Close()

	// Tee all output to both stdout and the markdown log file.
	w := io.MultiWriter(os.Stdout, logFile)

	fmt.Fprintf(w, "# Citation Eval Tool\n\n")
	fmt.Fprintf(w, "- **Enclave:** %s\n", *enclaveHost)
	fmt.Fprintf(w, "- **Repo:** %s\n", *repoName)
	fmt.Fprintf(w, "- **Model:** %s\n", *modelName)
	fmt.Fprintf(w, "- **Date:** %s\n", time.Now().Format("2006-01-02 15:04:05 MST"))

	fmt.Fprintf(w, "\nVerifying enclave attestation... ")
	sc := tinfoil.NewSecureClient(*enclaveHost, *repoName)
	httpClient, err := sc.HTTPClient()
	if err != nil {
		log.Fatalf("attestation failed: %v", err)
	}
	httpClient.Timeout = 5 * time.Minute
	fmt.Fprintf(w, "OK\n")

	baseURL := "https://" + *enclaveHost + "/v1"
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
	)

	active := scenarios
	if *numQueries > 0 && *numQueries < len(active) {
		active = active[:*numQueries]
	}

	totalReqs := len(resultFormats) * len(promptModes) * len(active)
	fmt.Fprintf(w, "- **Scenarios:** %d\n", len(active))
	fmt.Fprintf(w, "- **Formats:** %d (%s)\n", len(resultFormats), formatNames())
	fmt.Fprintf(w, "- **Prompts:** %d (%s)\n", len(promptModes), promptNames())
	fmt.Fprintf(w, "- **Total reqs:** %d\n\n", totalReqs)

	var cells []cell
	reqNum := 0

	fmt.Fprintf(w, "## Individual Results\n\n")

	for _, rf := range resultFormats {
		for _, mode := range promptModes {
			for _, sc := range active {
				reqNum++
				fmt.Fprintf(w, "### [%d/%d] format=%s prompt=%s\n\n",
					reqNum, totalReqs, rf.Name, mode.Name)
				fmt.Fprintf(w, "**Query:** %s\n\n", sc.Query)

				toolOutput := rf.Format(sc.Results)
				content, rawJSON, err := runQuery(context.Background(), client, mode.System, sc, toolOutput)
				c := cell{
					Format:  rf.Name,
					Prompt:  mode.Name,
					Query:   sc.Query,
					Content: content,
					RawJSON: rawJSON,
					Err:     err,
					Counts:  make([]int, len(patterns)),
				}

				if err != nil {
					fmt.Fprintf(w, "**ERROR:** %v\n\n", err)
					cells = append(cells, c)
					continue
				}

				if *rawOutput {
					fmt.Fprintf(w, "**Raw JSON:**\n```json\n%s\n```\n\n", rawJSON)
				}

				fmt.Fprintf(w, "**Response:**\n\n> %s\n\n", strings.ReplaceAll(content, "\n", "\n> "))

				// Scan for citation patterns.
				fmt.Fprintf(w, "**Patterns found:**\n\n")
				found := false
				for j, p := range patterns {
					matches := p.Re.FindAllString(content, -1)
					c.Counts[j] = len(matches)
					if len(matches) > 0 {
						found = true
						fmt.Fprintf(w, "- `%s` — %d match(es), e.g. `%s`\n", p.Name, len(matches), truncate(matches[0], 80))
					}
				}
				if !found {
					fmt.Fprintf(w, "- _(none)_\n")
				}
				fmt.Fprintf(w, "\n")

				cells = append(cells, c)
			}
		}
	}

	// Print aggregate matrix: one table per result format.
	fmt.Fprintf(w, "## Aggregate Matrix\n\n")

	for _, rf := range resultFormats {
		fmt.Fprintf(w, "### format=%s\n\n", rf.Name)

		// Header row.
		fmt.Fprintf(w, "| %-30s", "Pattern")
		for _, mode := range promptModes {
			fmt.Fprintf(w, " | %s", mode.Name)
		}
		fmt.Fprintf(w, " |\n")
		fmt.Fprintf(w, "|%s", strings.Repeat("-", 32))
		for range promptModes {
			fmt.Fprintf(w, "|%s", strings.Repeat("-", len("  cite-markdown  ")))
		}
		fmt.Fprintf(w, "|\n")

		// One row per pattern, summing across queries for each prompt mode.
		for j, p := range patterns {
			fmt.Fprintf(w, "| %-30s", p.Name)
			for _, mode := range promptModes {
				total := 0
				for _, c := range cells {
					if c.Format == rf.Name && c.Prompt == mode.Name {
						total += c.Counts[j]
					}
				}
				fmt.Fprintf(w, " | %d", total)
			}
			fmt.Fprintf(w, " |\n")
		}
		fmt.Fprintf(w, "\n")
	}

	// Per-cell detail with first examples.
	fmt.Fprintf(w, "## Per-Combo Detail\n\n")
	for _, rf := range resultFormats {
		for _, mode := range promptModes {
			fmt.Fprintf(w, "### format=%s prompt=%s\n\n", rf.Name, mode.Name)
			for _, p := range patterns {
				total := 0
				var example string
				for _, c := range cells {
					if c.Format != rf.Name || c.Prompt != mode.Name {
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
				fmt.Fprintf(w, "- `%s` — %d — e.g. `%s`\n", p.Name, total, truncate(example, 60))
			}
			fmt.Fprintf(w, "\n")
		}
	}

	fmt.Fprintf(w, "---\n\n_Log written to `%s`_\n", logPath)
}

// runQuery builds a prefilled conversation (user → assistant tool_call → tool result)
// and sends it to the model so it generates the final answer with citations.
func runQuery(ctx context.Context, client openai.Client, systemPrompt string, sc scenario, toolOutput string) (content string, rawJSON string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	const toolCallID = "call_eval_001"

	var messages []openai.ChatCompletionMessageParamUnion

	// Optional system prompt.
	if systemPrompt != "" {
		messages = append(messages, openai.SystemMessage(systemPrompt))
	}

	// User asks a question.
	messages = append(messages, openai.UserMessage(sc.Query))

	// Assistant decided to call the search tool.
	messages = append(messages, openai.ChatCompletionMessageParamUnion{
		OfAssistant: &openai.ChatCompletionAssistantMessageParam{
			ToolCalls: []openai.ChatCompletionMessageToolCallUnionParam{
				{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: toolCallID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      "search",
							Arguments: fmt.Sprintf(`{"query":"%s"}`, sc.SearchQuery),
						},
					},
				},
			},
		},
	})

	// Tool returns search results.
	messages = append(messages, openai.ToolMessage(toolOutput, toolCallID))

	// Declare the search tool so the chat template handles the history correctly.
	searchTool := openai.ChatCompletionToolUnionParam{
		OfFunction: &openai.ChatCompletionFunctionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        "search",
				Description: openai.String("Search the web for information."),
				Parameters: shared.FunctionParameters{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "The search query.",
						},
					},
					"required": []string{"query"},
				},
			},
		},
	}

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(*modelName),
		Messages: messages,
		Tools:    []openai.ChatCompletionToolUnionParam{searchTool},
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

func formatNames() string {
	names := make([]string, len(resultFormats))
	for i, f := range resultFormats {
		names[i] = f.Name
	}
	return strings.Join(names, ", ")
}

func promptNames() string {
	names := make([]string, len(promptModes))
	for i, m := range promptModes {
		names[i] = m.Name
	}
	return strings.Join(names, ", ")
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
