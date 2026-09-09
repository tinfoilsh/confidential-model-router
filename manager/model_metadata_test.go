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
	intelligence := map[string]ModelIntelligence{
		"scored-model": {Scores: entry.Intelligence, Reasoning: entry.ReasoningParams},
		"vision-model": {Scores: map[string]int{"on": 20}},
	}
	em.modelIntelligence.Store(&intelligence)
	em.multimodalModels.Store("vision-model", struct{}{})

	got, ok := em.ModelIntelligence("scored-model")
	if !ok || got.Scores["low"] != 30 {
		t.Fatalf("ModelIntelligence lookup = %+v, %v", got, ok)
	}
	if _, ok := em.ModelIntelligence("missing-model"); ok {
		t.Fatal("did not expect intelligence for missing model")
	}

	catalog := em.AutoRouteCatalog()
	if len(catalog) != 2 {
		t.Fatalf("catalog size = %d, want 2", len(catalog))
	}
	if catalog[0].Name != "scored-model" || catalog[1].Name != "vision-model" {
		t.Fatalf("catalog not sorted by name: %q, %q", catalog[0].Name, catalog[1].Name)
	}
	if catalog[0].Multimodal || !catalog[1].Multimodal {
		t.Fatalf("multimodal flags not joined onto catalog: %v, %v", catalog[0].Multimodal, catalog[1].Multimodal)
	}
	if catalog[0].Reasoning == nil || catalog[0].Reasoning.EffortMap["high"] != "max" {
		t.Fatalf("reasoning params not carried into catalog: %+v", catalog[0].Reasoning)
	}
}

func TestManagerHasHealthyEnclave(t *testing.T) {
	down := newTestModel("a")
	tripBreaker(down.Enclaves["a"])
	em := newTestManager(map[string]*Model{
		"down-model": down,
		"up-model":   newTestModel("b"),
	})
	if em.HasHealthyEnclave("down-model") {
		t.Fatal("tripped model reported healthy")
	}
	if !em.HasHealthyEnclave("up-model") {
		t.Fatal("healthy model reported unhealthy")
	}
	if em.HasHealthyEnclave("unknown-model") {
		t.Fatal("unknown model reported healthy")
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
