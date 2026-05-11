package manager

import "testing"

func TestTopLevelUsageExtractorExtractsUsageAcrossChunks(t *testing.T) {
	input := `{"data":"not a \"usage\" key","nested":{"usage":{"prompt_tokens":1,"completion_tokens":1}},"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`

	extractor := &topLevelUsageExtractor{}
	for i := 0; i < len(input); i += 5 {
		end := min(i+5, len(input))
		if _, err := extractor.Write([]byte(input[i:end])); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}

	usage := extractor.Usage()
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.PromptTokens != 7 {
		t.Fatalf("prompt tokens = %d, want 7", usage.PromptTokens)
	}
	if usage.CompletionTokens != 3 {
		t.Fatalf("completion tokens = %d, want 3", usage.CompletionTokens)
	}
	if usage.TotalTokens != 10 {
		t.Fatalf("total tokens = %d, want 10", usage.TotalTokens)
	}
}

func TestTopLevelUsageExtractorIgnoresUsageInStringValues(t *testing.T) {
	input := `{"data":"{\"usage\":{\"prompt_tokens\":999}}","message":"usage"}`

	extractor := &topLevelUsageExtractor{}
	if _, err := extractor.Write([]byte(input)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if extractor.Usage() != nil {
		t.Fatal("expected no usage")
	}
}
