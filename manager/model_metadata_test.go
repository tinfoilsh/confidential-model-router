package manager

import (
	"encoding/json"
	"testing"

	"github.com/tinfoilsh/confidential-model-router/config"
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

func TestAutoRouteStatus(t *testing.T) {
	down := newTestModel("a")
	tripBreaker(down.Enclaves["a"])
	em := newTestManager(map[string]*Model{
		"down-model": down,
		"up-model":   newTestModel("b"),
	})
	if eligible, healthy := em.AutoRouteStatus("down-model", ""); !eligible || healthy {
		t.Fatalf("tripped model status = (%v, %v), want eligible but unhealthy", eligible, healthy)
	}
	if eligible, healthy := em.AutoRouteStatus("up-model", ""); !eligible || !healthy {
		t.Fatalf("healthy model status = (%v, %v), want eligible and healthy", eligible, healthy)
	}
	if eligible, healthy := em.AutoRouteStatus("unknown-model", ""); eligible || healthy {
		t.Fatalf("unknown model status = (%v, %v), want ineligible and unhealthy", eligible, healthy)
	}
}

func TestAutoRouteStatusReservedPools(t *testing.T) {
	const modelName = "reserved-model"
	model := newTestModel("a", "b")
	model.applyReservations(modelName, []config.ReservationConfig{
		{OrgIDs: []string{"org-a"}, Enclaves: []string{"a"}},
		{OrgIDs: []string{"org-b"}, Enclaves: []string{"b"}},
	}, []string{"a", "b"})
	tripBreaker(model.Enclaves["a"])
	em := newTestManager(map[string]*Model{modelName: model})
	for _, tc := range []struct {
		orgID    string
		eligible bool
		healthy  bool
	}{
		{"org-a", true, false},
		{"org-b", true, true},
		{"org-other", false, false},
		{"", false, false},
	} {
		if eligible, healthy := em.AutoRouteStatus(modelName, tc.orgID); eligible != tc.eligible || healthy != tc.healthy {
			t.Errorf("org %q status = (%v, %v), want (%v, %v)", tc.orgID, eligible, healthy, tc.eligible, tc.healthy)
		}
	}

	model.applyReservations(modelName, []config.ReservationConfig{
		{OrgIDs: []string{"org-a"}, Enclaves: []string{"a"}},
	}, []string{"a", "b"})
	for _, orgID := range []string{"org-a", "org-other", ""} {
		if eligible, healthy := em.AutoRouteStatus(modelName, orgID); !eligible || !healthy {
			t.Errorf("org %q with healthy shared spill = (%v, %v), want eligible and healthy", orgID, eligible, healthy)
		}
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
