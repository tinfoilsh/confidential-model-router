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

type openAIModelEntry struct {
	ID         string `json:"id"`
	Multimodal bool   `json:"multimodal"`
	Type       string `json:"type"`
}

type openAIModelsList struct {
	Data []openAIModelEntry `json:"data"`
}

// IsMultimodal reports whether the named model accepts image content parts.
func (em *EnclaveManager) IsMultimodal(modelName string) bool {
	_, ok := em.multimodalModels.Load(modelName)
	return ok
}

// refreshMultimodalModels updates the sticky multimodal cache in the
// background. Best-effort: failures and missing entries leave the cache as-is.
func (em *EnclaveManager) refreshMultimodalModels() {
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

		for _, e := range parsed.Data {
			// Restrict to chat-shaped models so non-chat services that carry
			// multimodal:true don't route PDFs as page images.
			if e.ID == "" || !e.Multimodal || (e.Type != "" && e.Type != "chat") {
				continue
			}
			em.multimodalModels.Store(e.ID, struct{}{})
		}
	}()
}
