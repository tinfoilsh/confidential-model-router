package manager

import (
	"encoding/json"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

func TestRequestCostUSD(t *testing.T) {
	cachedPrice := 0.25
	tests := []struct {
		name     string
		usage    *tokencount.Usage
		pricing  ModelPricing
		expected string
	}{
		{
			name: "uncached input and output",
			usage: &tokencount.Usage{
				PromptTokens:     1_000,
				CompletionTokens: 500,
			},
			pricing: ModelPricing{
				InputTokenPricePer1M:  1.5,
				OutputTokenPricePer1M: 5.25,
			},
			expected: "0.004125",
		},
		{
			name: "cached input discount and request price",
			usage: &tokencount.Usage{
				PromptTokens:        1_000,
				CompletionTokens:    500,
				PromptTokensDetails: &tokencount.PromptTokensDetails{CachedTokens: 400},
			},
			pricing: ModelPricing{
				InputTokenPricePer1M:       1,
				OutputTokenPricePer1M:      2,
				CachedInputTokenPricePer1M: &cachedPrice,
				RequestPrice:               0.01,
			},
			expected: "0.0117",
		},
		{
			name: "cached input falls back to full input price",
			usage: &tokencount.Usage{
				PromptTokens:        1_000,
				PromptTokensDetails: &tokencount.PromptTokensDetails{CachedTokens: 400},
			},
			pricing: ModelPricing{
				InputTokenPricePer1M: 1,
			},
			expected: "0.001",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatRequestCostUSD(tt.usage, tt.pricing); got != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestModelPricingJSONAndLookup(t *testing.T) {
	var models openAIModelsList
	err := json.Unmarshal([]byte(`{
		"data": [{
			"id": "priced-model",
			"pricing": {
				"inputTokenPricePer1M": 1.5,
				"outputTokenPricePer1M": 5.25,
				"cachedInputTokenPricePer1M": 0.375,
				"requestPrice": 0.01
			}
		}]
	}`), &models)
	if err != nil {
		t.Fatal(err)
	}
	if len(models.Data) != 1 || models.Data[0].Pricing == nil {
		t.Fatal("expected model pricing to decode")
	}

	em := &EnclaveManager{}
	pricingByModel := map[string]ModelPricing{"priced-model": *models.Data[0].Pricing}
	em.modelPricing.Store(&pricingByModel)
	pricing, ok := em.ModelPricing("priced-model")
	if !ok {
		t.Fatal("expected cached model pricing")
	}
	if pricing.CachedInputTokenPricePer1M == nil || *pricing.CachedInputTokenPricePer1M != 0.375 {
		t.Fatalf("unexpected cached input price: %+v", pricing.CachedInputTokenPricePer1M)
	}
	if _, ok := em.ModelPricing("missing-model"); ok {
		t.Fatal("did not expect pricing for missing model")
	}
}
