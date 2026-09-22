package toolruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMarkerSourcesPreserveFullContentAndURLs(t *testing.T) {
	const sourceCount = 10
	const repetitions = 2000
	text := "\n  " + strings.Repeat("Full source text 🔎. ", repetitions) + "Important conclusion at the end.\n"
	var sources []toolCallSource
	for range sourceCount {
		sources = append(sources, toolCallSource{url: "https://example.com/page?q=exact#section", title: "Evidence", snippet: text})
	}
	encoded := encodeMarkerSources(sources)
	total := 0
	for _, source := range encoded {
		if source["url"] != sources[0].url {
			t.Fatal("source URL was altered")
		}
		snippet, _ := source["snippet"].(string)
		if snippet != text {
			t.Fatal("source content was altered or truncated")
		}
		total += len(snippet)
	}
	if total != len(text)*sourceCount || len(encoded) != sourceCount {
		t.Fatalf("source content or sources were dropped: %d, %d", total, len(encoded))
	}
	if _, err := json.Marshal(encoded); err != nil {
		t.Fatal(err)
	}
}

func TestFetchEvidenceIsAttributedToItsOwnURL(t *testing.T) {
	const firstURL = "https://example.com/first"
	const secondURL = "https://example.com/second"
	sources := toolCallSourcesForResult(routerFetchToolName, map[string]any{"pages": []any{
		map[string]any{"url": firstURL, "content": "First page evidence"},
		map[string]any{"url": secondURL, "content": "Second page evidence"},
	}}, "")
	markers := tinfoilEventMarkersForRecords([]toolCallRecord{{
		name:          routerFetchToolName,
		arguments:     map[string]any{"urls": []any{firstURL, secondURL}},
		resultSources: sources,
	}})
	completed := 0
	for _, match := range tinfoilEventMarkerPattern.FindAllStringSubmatch(markers, -1) {
		var event map[string]any
		if err := json.Unmarshal([]byte(match[1]), &event); err != nil {
			t.Fatal(err)
		}
		if event["status"] != "completed" {
			continue
		}
		completed++
		source := event["sources"].([]any)[0].(map[string]any)
		url := event["action"].(map[string]any)["url"]
		if source["url"] != url {
			t.Fatal("fetch evidence attached to the wrong page")
		}
		if source["snippet"] == "" {
			t.Fatal("fetch excerpt missing")
		}
	}
	if len(sources) != 2 || completed != 2 {
		t.Fatalf("expected both fetched pages and markers, got %d, %d", len(sources), completed)
	}
}

func TestStreamedFetchCarriesRawExcerptsWithoutHarmonyCursors(t *testing.T) {
	const url = "https://example.com/paper"
	const excerpt = "The paper reports 73 participants."
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "evidence-fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "fetch", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"pages": []any{map[string]any{"url": url, "content": excerpt}}}}, nil
	})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "evidence-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	registry, err := buildSessionRegistry(ctx, []Profile{WebSearch}, dialFromMap(map[string]*mcp.ClientSession{WebSearch.Name: session}))
	if err != nil {
		t.Fatal(err)
	}
	streamer, recorder := newTestChatStreamer(t)
	streamer.citations.Harmony = true
	streamer.eventFlags.webSearch = true
	execution, err := streamer.executeTool(ctx, registry, toolCall{name: routerFetchToolName, arguments: map[string]any{"urls": []any{url}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(execution.output, "[1] Fetched page") || len(execution.sources) != 1 {
		t.Fatalf("unexpected live output: %q", execution.output)
	}
	completed := 0
	for _, frame := range strings.Split(recorder.Body.String(), "\n\n") {
		if frame == "" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		choice := chunk["choices"].([]any)[0].(map[string]any)
		content := stringValue(choice["delta"].(map[string]any)["content"])
		for _, match := range tinfoilEventMarkerPattern.FindAllStringSubmatch(content, -1) {
			var event map[string]any
			if err := json.Unmarshal([]byte(match[1]), &event); err != nil {
				t.Fatal(err)
			}
			if event["status"] != "completed" {
				continue
			}
			completed++
			source := event["sources"].([]any)[0].(map[string]any)
			if source["url"] != url || source["snippet"] != excerpt {
				t.Fatalf("incorrect saved evidence: %#v", source)
			}
		}
	}
	if completed != 1 {
		t.Fatalf("expected one completed fetch marker, got %d", completed)
	}
}

func TestMarkerWithoutExcerptsRemainsCompatible(t *testing.T) {
	encoded := encodeMarkerSources([]toolCallSource{{url: "https://example.com", title: "Title"}})
	if len(encoded) != 1 || len(encoded[0]) != 2 {
		t.Fatalf("unexpected legacy source: %#v", encoded)
	}
}
