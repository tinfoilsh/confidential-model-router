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
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

var (
	baseURL    = flag.String("base-url", "https://gpt-oss-120b-0.inf9.tinfoil.sh/v1", "enclave base URL (raw vLLM endpoint)")
	modelName  = flag.String("model", "gpt-oss-120b", "model identifier")
	numQueries = flag.Int("queries", 0, "number of eval queries to run (0 = all)")
	rawOutput  = flag.Bool("raw", false, "print full raw response JSON")
)

// scenario is a user query paired with fake search results the model should cite.
type scenario struct {
	Query       string
	SearchQuery string // what the "tool" searched for
	Results     string // fake tool output with URLs and snippets
}

var scenarios = []scenario{
	{
		Query:       "What are the latest headlines today?",
		SearchQuery: "latest headlines today",
		Results: `[1] "Global Climate Summit Reaches Historic Agreement" - https://www.reuters.com/world/climate-summit-agreement-2026
World leaders at the 2026 Global Climate Summit in Berlin reached a historic agreement to cut emissions by 50% by 2035, with binding commitments from all major economies.

[2] "Tech Giants Report Record Q1 Earnings" - https://www.bloomberg.com/news/tech-q1-earnings-2026
Major technology companies including Apple, Microsoft, and Alphabet reported better-than-expected first quarter results, driven by AI infrastructure spending.

[3] "NBA Playoffs: Celtics Advance to Conference Finals" - https://www.espn.com/nba/story/celtics-advance-2026
The Boston Celtics defeated the Philadelphia 76ers 112-98 in Game 6 to advance to the Eastern Conference Finals.`,
	},
	{
		Query:       "Who won the most recent NBA game?",
		SearchQuery: "most recent NBA game results",
		Results: `[1] "Celtics 112, 76ers 98 - Game 6 Recap" - https://www.espn.com/nba/recap/_/gameId/401656789
Jayson Tatum scored 34 points and grabbed 11 rebounds as the Boston Celtics eliminated the Philadelphia 76ers with a 112-98 victory in Game 6 of their Eastern Conference semifinal series.

[2] "NBA Playoffs Bracket 2026" - https://www.nba.com/playoffs/2026/bracket
Full playoff bracket and results. Eastern Conference: Celtics vs 76ers (Celtics win 4-2). Western Conference: Thunder vs Nuggets (series tied 3-3).

[3] "Thunder-Nuggets Game 6 Preview" - https://www.espn.com/nba/preview/_/gameId/401656790
The Oklahoma City Thunder host the Denver Nuggets in a decisive Game 6, with the winner advancing to the Western Conference Finals.`,
	},
	{
		Query:       "What is the current price of Bitcoin?",
		SearchQuery: "current bitcoin price",
		Results: `[1] "Bitcoin Price Today" - https://www.coindesk.com/price/bitcoin
Bitcoin (BTC) is currently trading at $94,521.30, up 2.3% in the last 24 hours. Market cap: $1.87 trillion. 24h volume: $28.4 billion.

[2] "Bitcoin Surges Past $94K on ETF Inflows" - https://www.coindesk.com/markets/bitcoin-etf-inflows-2026
Bitcoin climbed above $94,000 on Monday as spot Bitcoin ETFs recorded $1.2 billion in net inflows last week, the highest weekly total since January.

[3] "Crypto Market Overview" - https://www.coingecko.com/en/coins/bitcoin
Bitcoin price: $94,521.30 USD. 24h change: +2.3%. 7d change: +8.1%. All-time high: $109,114 (Jan 2025).`,
	},
	{
		Query:       "Summarize the top 3 results for 'climate change 2026'",
		SearchQuery: "climate change 2026",
		Results: `[1] "2026 Global Climate Report: Temperatures Hit New Record" - https://www.nature.com/articles/climate-report-2026
The World Meteorological Organization confirmed that 2025 was the hottest year on record, with global mean temperature 1.55°C above pre-industrial levels. The report warns that without immediate action, 2°C will be breached by 2040.

[2] "Climate Change 2026: Policy Progress and Gaps" - https://www.unep.org/resources/climate-change-2026-policy
A comprehensive UN Environment Programme report finds that current national pledges would reduce emissions by only 15% by 2030, far short of the 43% needed. However, renewable energy deployment accelerated by 35% in 2025.

[3] "How Climate Change Is Reshaping Agriculture in 2026" - https://www.bbc.com/news/science-environment-climate-agriculture-2026
Extreme weather events in 2025 caused $180 billion in agricultural losses worldwide. Farmers in Southeast Asia and sub-Saharan Africa were disproportionately affected, with crop yields falling 20-30% in some regions.`,
	},
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

	apiKey := mustEnv("TINFOIL_API_KEY")

	httpClient := &http.Client{Timeout: 5 * time.Minute}
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(*baseURL),
		option.WithHTTPClient(httpClient),
	)

	active := scenarios
	if *numQueries > 0 && *numQueries < len(active) {
		active = active[:*numQueries]
	}

	fmt.Printf("Citation Eval Tool\n")
	fmt.Printf("==================\n")
	fmt.Printf("Endpoint:   %s\n", *baseURL)
	fmt.Printf("Model:      %s\n", *modelName)
	fmt.Printf("Scenarios:  %d\n", len(active))
	fmt.Printf("Prompts:    %d\n", len(promptModes))
	fmt.Printf("Total reqs: %d\n\n", len(active)*len(promptModes))

	var cells []cell

	for _, mode := range promptModes {
		for _, sc := range active {
			fmt.Printf("─── prompt=%s | query=%q ───\n", mode.Name, truncate(sc.Query, 50))

			content, rawJSON, err := runQuery(context.Background(), client, mode.System, sc)
			c := cell{
				Prompt:  mode.Name,
				Query:   sc.Query,
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
		fmt.Printf(" | %13s", mode.Name)
	}
	fmt.Println()
	fmt.Printf("%s", strings.Repeat("-", 30))
	for range promptModes {
		fmt.Printf("-|-%s", strings.Repeat("-", 13))
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
			fmt.Printf(" | %13d", total)
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

// runQuery builds a prefilled conversation (user → assistant tool_call → tool result)
// and sends it to the model so it generates the final answer with citations.
func runQuery(ctx context.Context, client openai.Client, systemPrompt string, sc scenario) (content string, rawJSON string, err error) {
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
	messages = append(messages, openai.ToolMessage(sc.Results, toolCallID))

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
