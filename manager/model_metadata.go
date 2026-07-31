package manager

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const modelMetadataHTTPTimeout = 10 * time.Second

type ModelPricing struct {
	InputTokenPricePer1M       float64  `json:"inputTokenPricePer1M"`
	OutputTokenPricePer1M      float64  `json:"outputTokenPricePer1M"`
	CachedInputTokenPricePer1M *float64 `json:"cachedInputTokenPricePer1M,omitempty"`
	RequestPrice               float64  `json:"requestPrice"`
}

type openAIModelEntry struct {
	ID         string        `json:"id"`
	Multimodal bool          `json:"multimodal"`
	Type       string        `json:"type"`
	Pricing    *ModelPricing `json:"pricing"`
}

type openAIModelsList struct {
	Data []openAIModelEntry `json:"data"`
}

// IsMultimodal reports whether the named model accepts image content parts.
func (em *EnclaveManager) IsMultimodal(modelName string) bool {
	_, ok := em.multimodalModels.Load(modelName)
	return ok
}

// ModelPricing returns the latest published prices for a model.
func (em *EnclaveManager) ModelPricing(modelName string) (ModelPricing, bool) {
	if em == nil {
		return ModelPricing{}, false
	}
	pricing := em.modelPricing.Load()
	if pricing == nil {
		return ModelPricing{}, false
	}
	value, ok := (*pricing)[modelName]
	return value, ok
}

func (p ModelPricing) valid() bool {
	if p.InputTokenPricePer1M < 0 || p.OutputTokenPricePer1M < 0 || p.RequestPrice < 0 {
		return false
	}
	return p.CachedInputTokenPricePer1M == nil || *p.CachedInputTokenPricePer1M >= 0
}

// refreshModelMetadata updates the model pricing and sticky multimodal cache
// in the background. Best-effort: failures leave both caches as-is.
func (em *EnclaveManager) refreshModelMetadata() {
	if em.controlPlaneURL == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), modelMetadataHTTPTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, em.controlPlaneURL+"/v1/models", nil)
		if err != nil {
			log.Debugf("multimodal refresh: build request: %v", err)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Debugf("multimodal refresh: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			log.Debugf("multimodal refresh: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			return
		}

		var parsed openAIModelsList
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			log.Debugf("multimodal refresh: decode: %v", err)
			return
		}

		pricing := make(map[string]ModelPricing, len(parsed.Data))
		for _, e := range parsed.Data {
			if e.ID != "" && e.Pricing != nil && e.Pricing.valid() {
				pricing[e.ID] = *e.Pricing
			}
			// Restrict to chat-shaped models so non-chat services that carry
			// multimodal:true don't route PDFs as page images.
			if e.ID == "" || !e.Multimodal || (e.Type != "" && e.Type != "chat") {
				continue
			}
			em.multimodalModels.Store(e.ID, struct{}{})
		}
		em.modelPricing.Store(&pricing)
	}()
}
