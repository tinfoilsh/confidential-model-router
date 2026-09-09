package manager

import (
	"encoding/json"
	"testing"
)

func TestModelIntelligenceJSONAndLookup(t *testing.T) {
	var models openAIModelsList
	err := json.Unmarshal([]byte(`{
		"data": [{
			"id": "scored-model",
			"type": "chat",
			"intelligence": {"low": 30, "high": 42},
			"reasoning_params": {
				"params": {
					"/v1/chat/completions": {
						"enable": {"chat_template_kwargs": {"reasoning_effort": "$EFFORT"}}
					}
				},
				"effort_map": {"low": "low", "high": "max"}
			}
		}]
	}`), &models)
	if err != nil {
		t.Fatal(err)
	}
	if len(models.Data) != 1 {
		t.Fatalf("expected one model, got %d", len(models.Data))
	}
	entry := models.Data[0]
	if entry.Intelligence["high"] != 42 {
		t.Fatalf("intelligence.high = %d, want 42", entry.Intelligence["high"])
	}
	if entry.ReasoningParams == nil || entry.ReasoningParams.EffortMap["high"] != "max" {
		t.Fatalf("reasoning params did not decode: %+v", entry.ReasoningParams)
	}
	enable := entry.ReasoningParams.Params["/v1/chat/completions"].Enable
	kwargs, _ := enable["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != "$EFFORT" {
		t.Fatalf("enable fragment did not decode: %v", enable)
	}

	em := &EnclaveManager{}
	catalog := map[string]ModelIntelligence{
		"scored-model": {Scores: entry.Intelligence, Reasoning: entry.ReasoningParams},
	}
	em.modelIntelligence.Store(&catalog)
	got, ok := em.ModelIntelligence("scored-model")
	if !ok || got.Scores["low"] != 30 {
		t.Fatalf("ModelIntelligence lookup = %+v, %v", got, ok)
	}
	if _, ok := em.ModelIntelligence("missing-model"); ok {
		t.Fatal("did not expect intelligence for missing model")
	}
	if len(em.IntelligenceCatalog()) != 1 {
		t.Fatalf("catalog size = %d, want 1", len(em.IntelligenceCatalog()))
	}
}

func TestValidIntelligence(t *testing.T) {
	cases := []struct {
		name   string
		scores map[string]int
		want   bool
	}{
		{"nil", nil, false},
		{"empty", map[string]int{}, false},
		{"in range", map[string]int{"off": 0, "high": 100}, true},
		{"negative", map[string]int{"low": -1}, false},
		{"over max", map[string]int{"high": 101}, false},
	}
	for _, tc := range cases {
		if got := validIntelligence(tc.scores); got != tc.want {
			t.Errorf("%s: validIntelligence = %v, want %v", tc.name, got, tc.want)
		}
	}
}
