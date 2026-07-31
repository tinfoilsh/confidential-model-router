package manager

import (
	"strconv"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

const (
	pricingTokenUnit  = 1_000_000
	costDecimalPlaces = 12
)

func requestCostUSD(usage *tokencount.Usage, pricing ModelPricing) float64 {
	cachedPromptTokens, _ := usage.CachedPromptTokens()
	uncachedPromptTokens := max(0, usage.PromptTokens-cachedPromptTokens)
	cachedInputPrice := pricing.InputTokenPricePer1M
	if pricing.CachedInputTokenPricePer1M != nil {
		cachedInputPrice = *pricing.CachedInputTokenPricePer1M
	}

	return pricing.RequestPrice +
		float64(uncachedPromptTokens)*pricing.InputTokenPricePer1M/pricingTokenUnit +
		float64(cachedPromptTokens)*cachedInputPrice/pricingTokenUnit +
		float64(usage.CompletionTokens)*pricing.OutputTokenPricePer1M/pricingTokenUnit
}

func formatRequestCostUSD(usage *tokencount.Usage, pricing ModelPricing) string {
	formatted := strconv.FormatFloat(requestCostUSD(usage, pricing), 'f', costDecimalPlaces, 64)
	formatted = strings.TrimRight(formatted, "0")
	formatted = strings.TrimRight(formatted, ".")
	if formatted == "" {
		return "0"
	}
	return formatted
}
