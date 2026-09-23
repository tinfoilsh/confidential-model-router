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

// WebSearchUsage describes the router-owned web search activity behind a
// request. The websearch service bills one session fee per request the first
// time the model invokes search or fetch, so the fee applies once when Calls
// is positive regardless of how many calls followed. SessionPricing is the
// websearch tool's published pricing; nil means the fee is unknown.
//
// Services lists the auxiliary services that ran inside the tool loop and
// bill per call rather than per session (the privacy filter, for example).
// Their fees are summed into other_cost_usd so the trailer format does not
// grow a field per service.
type WebSearchUsage struct {
	Calls          int
	SessionPricing *ModelPricing
	Services       []ServiceUsage
}

// ServiceUsage records how many times a per-call-priced service ran during a
// request. Pricing is the service's published pricing from the model
// catalog; nil means the fee is unknown and cost_usd must be omitted.
type ServiceUsage struct {
	Calls   int
	Pricing *ModelPricing
}

func (w *WebSearchUsage) billed() bool {
	return w != nil && w.Calls > 0
}

func (w *WebSearchUsage) costKnown() bool {
	if w == nil {
		return true
	}
	if w.billed() && w.SessionPricing == nil {
		return false
	}
	for _, s := range w.Services {
		if s.Calls > 0 && s.Pricing == nil {
			return false
		}
	}
	return true
}

func (w *WebSearchUsage) costNanos() int64 {
	if !w.billed() {
		return 0
	}
	return requestPriceNanos(w.SessionPricing.RequestPrice)
}

// otherBilled reports whether any per-call service ran at least once.
func (w *WebSearchUsage) otherBilled() bool {
	if w == nil {
		return false
	}
	for _, s := range w.Services {
		if s.Calls > 0 {
			return true
		}
	}
	return false
}

// otherCostNanos sums the per-call service fees. Callers must check
// costKnown first; a service with calls but no pricing contributes zero here.
func (w *WebSearchUsage) otherCostNanos() int64 {
	if w == nil {
		return 0
	}
	var total int64
	for _, s := range w.Services {
		if s.Calls > 0 && s.Pricing != nil {
			total += int64(s.Calls) * requestPriceNanos(s.Pricing.RequestPrice)
		}
	}
	return total
}

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
	return formatNanosUSD(requestCostNanos(usage, pricing))
}

func formatNanosUSD(costNanos int64) string {
	wholeDollars := costNanos / nanosPerDollar
	fractionalNanos := costNanos % nanosPerDollar
	if fractionalNanos == 0 {
		return strconv.FormatInt(wholeDollars, 10)
	}
	fraction := strconv.FormatInt(fractionalNanos, 10)
	fraction = strings.Repeat("0", nanodollarDecimalPlaces-len(fraction)) + fraction
	return strconv.FormatInt(wholeDollars, 10) + "." + strings.TrimRight(fraction, "0")
}
