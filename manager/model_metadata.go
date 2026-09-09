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

const (
	modelMetadataHTTPTimeout = 10 * time.Second

	// maxIntelligenceScore is the upper bound of the Artificial Analysis
	// Intelligence Index, which the control plane publishes per model and
	// reasoning setting.
	maxIntelligenceScore = 100
)

type ModelPricing struct {
	InputTokenPricePer1M       float64  `json:"inputTokenPricePer1M"`
	OutputTokenPricePer1M      float64  `json:"outputTokenPricePer1M"`
	CachedInputTokenPricePer1M *float64 `json:"cachedInputTokenPricePer1M,omitempty"`
	RequestPrice               float64  `json:"requestPrice"`
}

// ReasoningEndpointParams holds the request fragments that switch a model's
// reasoning on or off for one API endpoint. Enable may contain the literal
// "$EFFORT" placeholder that the caller substitutes with the native effort.
type ReasoningEndpointParams struct {
	Enable  map[string]any `json:"enable"`
	Disable map[string]any `json:"disable"`
}

// ReasoningParams describes how to apply a reasoning setting to a request for
// one model, keyed by endpoint path. EffortMap translates the client-facing
// effort key (low, medium, high) to the model's native effort value.
type ReasoningParams struct {
	Params    map[string]ReasoningEndpointParams `json:"params"`
	EffortMap map[string]string                  `json:"effort_map"`
}

// ModelIntelligence describes how capable a chat model is under each
// reasoning setting a client can select (off, on, low, medium, high), and how
// to apply that setting to a request.
type ModelIntelligence struct {
	Scores    map[string]int
	Reasoning *ReasoningParams
}

type openAIModelEntry struct {
	ID              string           `json:"id"`
	Multimodal      bool             `json:"multimodal"`
	Type            string           `json:"type"`
	Pricing         *ModelPricing    `json:"pricing"`
	Intelligence    map[string]int   `json:"intelligence"`
	ReasoningParams *ReasoningParams `json:"reasoning_params"`
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

// ModelIntelligence returns the per-effort intelligence scores and reasoning
// parameters for a chat model, if the control plane publishes them.
func (em *EnclaveManager) ModelIntelligence(modelName string) (ModelIntelligence, bool) {
	intelligence := em.modelIntelligence.Load()
	if intelligence == nil {
		return ModelIntelligence{}, false
	}
	value, ok := (*intelligence)[modelName]
	return value, ok
}

// IntelligenceCatalog returns every model with published intelligence scores.
// The map is a snapshot and must not be mutated.
func (em *EnclaveManager) IntelligenceCatalog() map[string]ModelIntelligence {
	intelligence := em.modelIntelligence.Load()
	if intelligence == nil {
		return nil
	}
	return *intelligence
}

// validIntelligence reports whether every published score is inside the
// Artificial Analysis Intelligence Index range.
func validIntelligence(scores map[string]int) bool {
	if len(scores) == 0 {
		return false
	}
	for _, score := range scores {
		if score < 0 || score > maxIntelligenceScore {
			return false
		}
	}
	return true
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
		intelligence := make(map[string]ModelIntelligence, len(parsed.Data))
		for _, e := range parsed.Data {
			if e.ID != "" && e.Pricing != nil && e.Pricing.valid() {
				pricing[e.ID] = *e.Pricing
			}
			// Restrict to chat-shaped models so non-chat services that carry
			// multimodal:true don't route PDFs as page images.
			if e.ID == "" || (e.Type != "" && e.Type != "chat") {
				continue
			}
			if validIntelligence(e.Intelligence) {
				intelligence[e.ID] = ModelIntelligence{Scores: e.Intelligence, Reasoning: e.ReasoningParams}
			}
			if e.Multimodal {
				em.multimodalModels.Store(e.ID, struct{}{})
			}
		}
		em.modelPricing.Store(&pricing)
		em.modelIntelligence.Store(&intelligence)
	}()
}
