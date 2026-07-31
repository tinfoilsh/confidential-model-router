package manager

import (
	"math"
	"strconv"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

const (
	pricingTokenUnit      = 1_000_000
	costSignificantDigits = 15
)

// CostKnownWithoutUsage reports whether request price alone determines cost.
func (p ModelPricing) CostKnownWithoutUsage() bool {
	if p.InputTokenPricePer1M != 0 || p.OutputTokenPricePer1M != 0 {
		return false
	}
	return p.CachedInputTokenPricePer1M == nil || *p.CachedInputTokenPricePer1M == 0
}

func requestCostUSD(usage *tokencount.Usage, pricing ModelPricing) float64 {
	cachedPromptTokens, _ := usage.CachedPromptTokens()
	cachedPromptTokens = max(0, cachedPromptTokens)
	uncachedPromptTokens := max(0, usage.PromptTokens-cachedPromptTokens)
	completionTokens := max(0, usage.CompletionTokens)
	cachedInputPrice := pricing.InputTokenPricePer1M
	if pricing.CachedInputTokenPricePer1M != nil {
		cachedInputPrice = *pricing.CachedInputTokenPricePer1M
	}

	return pricing.RequestPrice +
		float64(uncachedPromptTokens)*pricing.InputTokenPricePer1M/pricingTokenUnit +
		float64(cachedPromptTokens)*cachedInputPrice/pricingTokenUnit +
		float64(completionTokens)*pricing.OutputTokenPricePer1M/pricingTokenUnit
}

func formatRequestCostUSD(usage *tokencount.Usage, pricing ModelPricing) string {
	cost := requestCostUSD(usage, pricing)
	if cost == 0 {
		return "0"
	}
	decimalPlaces := max(0, costSignificantDigits-1-int(math.Floor(math.Log10(math.Abs(cost)))))
	formatted := strconv.FormatFloat(cost, 'f', decimalPlaces, 64)
	if decimalPlaces == 0 {
		return formatted
	}
	formatted = strings.TrimRight(formatted, "0")
	return strings.TrimRight(formatted, ".")
}
