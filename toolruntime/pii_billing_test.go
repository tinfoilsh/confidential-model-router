package toolruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tinfoilsh/confidential-model-router/toolruntime/citations"
)

func TestPIIBillingRequiresReceipt(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]any
		err  error
		want serviceBilling
	}{
		{"legacy detection not proof of billing", map[string]any{"pii_checked": true}, nil, serviceBilling{unknown: true}},
		{"legacy unchecked", map[string]any{"pii_checked": false}, nil, serviceBilling{}},
		{"receipt without detection", map[string]any{piiFilterRequestsField: float64(1), "pii_masked": false}, nil, serviceBilling{calls: 1}},
		{"receipt on error", map[string]any{piiFilterRequestsField: float64(1)}, errors.New("search failed"), serviceBilling{calls: 1}},
		{"explicit unbilled error", map[string]any{piiFilterRequestsField: float64(0)}, errors.New("inference failed"), serviceBilling{}},
		{"unknown receipt", map[string]any{piiFilterRequestsField: nil}, nil, serviceBilling{unknown: true}},
		{"transport failure", nil, errors.New("connection lost"), serviceBilling{unknown: true}},
		{"fraction rejected", map[string]any{piiFilterRequestsField: 0.5}, nil, serviceBilling{unknown: true}},
		{"invalid count", map[string]any{piiFilterRequestsField: 99}, nil, serviceBilling{unknown: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := piiBillingFromStructured(routerSearchToolName, tc.data, tc.err); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if got := piiBillingFromStructured(routerFetchToolName, tc.data, tc.err); got != (serviceBilling{}) {
				t.Fatal("fetch must not claim PII fees")
			}
		})
	}
}

func TestMCPErrorReceiptSurvivesBothRouterPaths(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "billing-test", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: mcpSearchToolName, InputSchema: &jsonschema.Schema{Type: "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "search failed"}}, StructuredContent: map[string]any{piiFilterRequestsField: 1}}, nil
	})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	registry, err := buildSessionRegistry(ctx, []Profile{WebSearch}, dialFromMap(map[string]*mcp.ClientSession{WebSearch.Name: session}))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.CloseAll()
	call := toolCall{name: routerSearchToolName, arguments: map[string]any{"query": "example"}}
	log := &toolCallLog{}
	executeRouterToolCall(ctx, registry, call, webSearchOptions{}, nil, &citations.State{}, log, "test", "")
	if log.piiFilterCalls() != 1 || log.records[0].errorReason == "" {
		t.Fatalf("non-streaming lost receipt: %+v", log.records)
	}
	streamer, _ := newTestResponsesStreamerForSpecEvents(t)
	streamLog := &toolCallLog{}
	resolveStreamingRouterToolCall(ctx, call, webSearchOptions{}, nil, streamLog, func(ctx context.Context, call toolCall) (toolExecution, error) {
		return executeToolWithProgress(ctx, registry, &citations.State{}, &responsesToolProgressEmitter{streamer: streamer}, call)
	}, "test", "")
	if streamLog.piiFilterCalls() != 1 || streamLog.records[0].errorReason == "" {
		t.Fatalf("streaming lost receipt: %+v", streamLog.records)
	}
}
