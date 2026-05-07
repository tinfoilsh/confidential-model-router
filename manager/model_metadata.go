package manager

import (
	"context"
	"encoding/json"
	"fmt"
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

// fetchMultimodalFlags returns a model-name -> multimodal map from the control
// plane's /v1/models endpoint. We restrict the flag to chat-shaped models so
// non-chat services (e.g. doc-upload) that happen to carry multimodal:true
// can't accidentally route PDFs as page images.
func fetchMultimodalFlags(ctx context.Context, controlPlaneURL string) (map[string]bool, error) {
	if strings.TrimSpace(controlPlaneURL) == "" {
		return nil, fmt.Errorf("controlPlaneURL is empty")
	}

	fetchCtx, cancel := context.WithTimeout(ctx, modelMetadataHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, controlPlaneURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("control plane returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed openAIModelsList
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(parsed.Data))
	for _, entry := range parsed.Data {
		if entry.ID == "" {
			continue
		}
		out[entry.ID] = entry.Multimodal && (entry.Type == "" || entry.Type == "chat")
	}
	return out, nil
}

// refreshModelMetadata stamps each known *Model with its multimodal flag. A
// transient control-plane failure preserves the previous cache rather than
// flipping flags, so requests in flight don't change behavior mid-sync.
func (em *EnclaveManager) refreshModelMetadata(ctx context.Context) error {
	if em.controlPlaneURL == "" {
		return nil
	}
	flags, err := fetchMultimodalFlags(ctx, em.controlPlaneURL)
	if err != nil {
		return err
	}
	em.models.Range(func(key, value any) bool {
		modelName, _ := key.(string)
		model, _ := value.(*Model)
		if model == nil {
			return true
		}
		if v, ok := flags[modelName]; ok {
			model.mu.Lock()
			model.Multimodal = v
			model.mu.Unlock()
		}
		return true
	})
	log.Debugf("refreshed model metadata for %d model(s) from control plane", len(flags))
	return nil
}

// IsMultimodal reports whether the named model accepts image content parts.
// Unknown or not-yet-synced models return false (safe default for routing
// PDF attachments).
func (em *EnclaveManager) IsMultimodal(modelName string) bool {
	model, found := em.GetModel(modelName)
	if !found {
		return false
	}
	model.mu.RLock()
	defer model.mu.RUnlock()
	return model.Multimodal
}
