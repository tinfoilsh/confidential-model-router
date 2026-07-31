package manager

import (
	"math"
	"strconv"
	"strings"

	"github.com/tinfoilsh/confidential-model-router/tokencount"
)

const (
	nanosPerDollar          = 1_000_000_000
	nanosPerTokenMultiplier = 1_000
	nanodollarDecimalPlaces = 9
)

// CostKnownWithoutUsage reports whether request price alone determines cost.
func (p ModelPricing) CostKnownWithoutUsage() bool {
	if tokenPriceNanos(p.InputTokenPricePer1M) != 0 || tokenPriceNanos(p.OutputTokenPricePer1M) != 0 {
		return false
	}
	return p.CachedInputTokenPricePer1M == nil || tokenPriceNanos(*p.CachedInputTokenPricePer1M) == 0
}

func tokenPriceNanos(pricePer1M float64) int64 {
	return int64(math.Round(pricePer1M * nanosPerTokenMultiplier))
}

func requestPriceNanos(price float64) int64 {
	return int64(math.Round(price * nanosPerDollar))
}

func requestCostNanos(usage *tokencount.Usage, pricing ModelPricing) int64 {
	cachedPromptTokens, _ := usage.CachedPromptTokens()
	cachedPromptTokens = max(0, cachedPromptTokens)
	uncachedPromptTokens := max(0, usage.PromptTokens-cachedPromptTokens)
	completionTokens := max(0, usage.CompletionTokens)
	cachedInputPriceNanos := tokenPriceNanos(pricing.InputTokenPricePer1M)
	if pricing.CachedInputTokenPricePer1M != nil {
		cachedInputPriceNanos = tokenPriceNanos(*pricing.CachedInputTokenPricePer1M)
	}

	return requestPriceNanos(pricing.RequestPrice) +
		int64(uncachedPromptTokens)*tokenPriceNanos(pricing.InputTokenPricePer1M) +
		int64(cachedPromptTokens)*cachedInputPriceNanos +
		int64(completionTokens)*tokenPriceNanos(pricing.OutputTokenPricePer1M)
}

func formatRequestCostUSD(usage *tokencount.Usage, pricing ModelPricing) string {
	costNanos := requestCostNanos(usage, pricing)
	wholeDollars := costNanos / nanosPerDollar
	fractionalNanos := costNanos % nanosPerDollar
	if fractionalNanos == 0 {
		return strconv.FormatInt(wholeDollars, 10)
	}
	fraction := strconv.FormatInt(fractionalNanos, 10)
	fraction = strings.Repeat("0", nanodollarDecimalPlaces-len(fraction)) + fraction
	return strconv.FormatInt(wholeDollars, 10) + "." + strings.TrimRight(fraction, "0")
}
