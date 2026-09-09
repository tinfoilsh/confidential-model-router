package autoroute

import (
	"net/http"
	"reflect"
	"testing"
)

func TestParseIntelligence(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		body    map[string]any
		want    int
		wantErr bool
	}{
		{name: "default when nothing set", body: map[string]any{}, want: DefaultIntelligence},
		{name: "header", header: "72", body: map[string]any{}, want: 72},
		{name: "header with whitespace", header: " 30 ", body: map[string]any{}, want: 30},
		{name: "header bounds", header: "100", body: map[string]any{}, want: 100},
		{name: "header over max", header: "101", body: map[string]any{}, wantErr: true},
		{name: "header negative", header: "-1", body: map[string]any{}, wantErr: true},
		{name: "header not a number", header: "high", body: map[string]any{}, wantErr: true},
		{
			name:   "body object wins over header",
			header: "10",
			body:   map[string]any{OptionsField: map[string]any{IntelligenceKey: float64(90)}},
			want:   90,
		},
		{
			name:    "body fractional rejected",
			body:    map[string]any{OptionsField: map[string]any{IntelligenceKey: 42.5}},
			wantErr: true,
		},
		{
			name:    "body string rejected",
			body:    map[string]any{OptionsField: map[string]any{IntelligenceKey: "42"}},
			wantErr: true,
		},
		{
			name:    "body out of range rejected",
			body:    map[string]any{OptionsField: map[string]any{IntelligenceKey: float64(250)}},
			wantErr: true,
		},
		{
			name:   "legacy array ignored, header used",
			header: "25",
			body:   map[string]any{OptionsField: []any{map[string]any{"model": "old"}}},
			want:   25,
		},
		{
			name: "object without intelligence key falls back to default",
			body: map[string]any{OptionsField: map[string]any{"other": true}},
			want: DefaultIntelligence,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			if tc.header != "" {
				header.Set(IntelligenceHeader, tc.header)
			}
			got, err := ParseIntelligence(header, tc.body)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got level %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("level = %d, want %d", got, tc.want)
			}
			if _, present := tc.body[OptionsField]; present {
				t.Fatalf("%s must be stripped from the body", OptionsField)
			}
		})
	}
}

func TestHasVisualInput(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{name: "string content", body: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{name: "text parts only", body: map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "text", "text": "hi"}}}}}},
		{name: "chat image", body: map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "image_url"}}}}}, want: true},
		{name: "chat file", body: map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "file"}}}}}, want: true},
		{name: "responses image", body: map[string]any{"input": []any{map[string]any{"content": []any{map[string]any{"type": "input_image"}}}}}, want: true},
		{name: "responses file", body: map[string]any{"input": []any{map[string]any{"content": []any{map[string]any{"type": "input_file"}}}}}, want: true},
		{name: "responses string input", body: map[string]any{"input": "hello"}},
		{name: "image in earlier turn", body: map[string]any{"messages": []any{
			map[string]any{"content": []any{map[string]any{"type": "image_url"}}},
			map[string]any{"content": "and now text"},
		}}, want: true},
		{name: "empty body", body: map[string]any{}},
	}
	for _, tc := range cases {
		if got := HasVisualInput(tc.body); got != tc.want {
			t.Errorf("%s: HasVisualInput = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func testCatalog() []Model {
	return []Model{
		{Name: "smart-text", Scores: map[string]int{"low": 39, "medium": 42, "high": 45}},
		{Name: "smart-vision", Multimodal: true, Scores: map[string]int{"on": 44}},
		{Name: "fast-vision", Multimodal: true, Scores: map[string]int{"low": 36, "medium": 39, "high": 42}},
		{Name: "tiny-text", Scores: map[string]int{"off": 8}},
		{Name: "toggle-vision", Multimodal: true, Scores: map[string]int{"off": 14, "on": 15}},
	}
}

func TestRankOrdersByDistanceToTarget(t *testing.T) {
	// Catalog max is 45, so normalized levels are round(score*100/45).
	ranked := Rank(testCatalog(), 0, false)
	if len(ranked) == 0 {
		t.Fatal("expected candidates")
	}
	if top := ranked[0]; top.Model.Name != "tiny-text" || top.Effort != "off" {
		t.Fatalf("top candidate for target 0 = %s/%s, want tiny-text/off", top.Model.Name, top.Effort)
	}

	// Candidates inside the fit band all come first; after them every
	// candidate must be no further from target than the one after it.
	for target := MinIntelligence; target <= MaxIntelligence; target += 10 {
		ranked = Rank(testCatalog(), target, false)
		best := MaxIntelligence
		for _, c := range ranked {
			if d := abs(c.Level - target); d < best {
				best = d
			}
		}
		firstOutside := len(ranked)
		for i, c := range ranked {
			if abs(c.Level-target) > best+FitTolerance {
				firstOutside = i
				break
			}
		}
		for i := firstOutside; i < len(ranked); i++ {
			if abs(ranked[i].Level-target) <= best+FitTolerance {
				t.Fatalf("target %d: in-band candidate %d sorted after an out-of-band one", target, i)
			}
			if i > firstOutside && abs(ranked[i-1].Level-target) > abs(ranked[i].Level-target) {
				t.Fatalf("target %d: candidate %d is further than candidate %d", target, i-1, i)
			}
		}
	}
}

func TestRankPrefersMultimodalWithinFitTolerance(t *testing.T) {
	// smart-text/high (45 -> 100) is the exact fit for target 100, but
	// smart-vision/on (44 -> 98) sits inside FitTolerance and is multimodal,
	// so it must win; the text model follows as the nearest remaining fit.
	ranked := Rank(testCatalog(), 100, false)
	if top := ranked[0]; top.Model.Name != "smart-vision" || top.Effort != "on" {
		t.Fatalf("top candidate for target 100 = %s/%s, want smart-vision/on", top.Model.Name, top.Effort)
	}
	if second := ranked[1]; second.Model.Name != "smart-text" || second.Effort != "high" {
		t.Fatalf("second candidate for target 100 = %s/%s, want smart-text/high", second.Model.Name, second.Effort)
	}

	// A multimodal candidate just outside the band must not jump ahead: at
	// target 87 the band is [84, 90], holding smart-text/low (87) and
	// fast-vision/medium (87); fast-vision/high (93) is outside it.
	ranked = Rank(testCatalog(), 87, false)
	if top := ranked[0]; top.Model.Name != "fast-vision" || top.Effort != "medium" {
		t.Fatalf("top candidate for target 87 = %s/%s, want fast-vision/medium", top.Model.Name, top.Effort)
	}
	if second := ranked[1]; second.Model.Name != "smart-text" || second.Effort != "low" {
		t.Fatalf("second candidate for target 87 = %s/%s, want smart-text/low", second.Model.Name, second.Effort)
	}
}

func TestRankPrefersMultimodalOnTies(t *testing.T) {
	// smart-text/medium (42) and fast-vision/high (42) share a score; the
	// multimodal model must sort first.
	ranked := Rank(testCatalog(), normalize(42, 45), false)
	if ranked[0].Model.Name != "fast-vision" || ranked[0].Effort != "high" {
		t.Fatalf("expected fast-vision/high first on tie, got %s/%s", ranked[0].Model.Name, ranked[0].Effort)
	}
	if ranked[1].Model.Name != "smart-text" || ranked[1].Effort != "medium" {
		t.Fatalf("expected smart-text/medium second on tie, got %s/%s", ranked[1].Model.Name, ranked[1].Effort)
	}
}

func TestRankRequireMultimodalExcludesTextOnlyModels(t *testing.T) {
	ranked := Rank(testCatalog(), 100, true)
	for _, c := range ranked {
		if !c.Model.Multimodal {
			t.Fatalf("text-only model %s ranked despite requireMultimodal", c.Model.Name)
		}
	}
	if top := ranked[0]; top.Model.Name != "smart-vision" {
		t.Fatalf("top multimodal candidate for target 100 = %s, want smart-vision", top.Model.Name)
	}
	if len(ranked) != 6 {
		t.Fatalf("expected 6 multimodal candidates, got %d", len(ranked))
	}
}

func TestRankIsDeterministic(t *testing.T) {
	first := Rank(testCatalog(), 50, false)
	for i := 0; i < 20; i++ {
		again := Rank(testCatalog(), 50, false)
		if !reflect.DeepEqual(first, again) {
			t.Fatal("Rank produced a different order on repeated calls")
		}
	}
}

func TestRankEmptyCatalog(t *testing.T) {
	if got := Rank(nil, 50, false); got != nil {
		t.Fatalf("expected nil for empty catalog, got %v", got)
	}
	if got := Rank([]Model{{Name: "unscored", Scores: map[string]int{}}}, 50, false); got != nil {
		t.Fatalf("expected nil for catalog without scores, got %v", got)
	}
}

func effortReasoning() *Reasoning {
	return &Reasoning{
		EffortMap: map[string]string{"low": "low", "medium": "high", "high": "max"},
		Params: map[string]EndpointParams{
			"/v1/chat/completions": {
				Enable: map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": effortPlaceholder, "clear_thinking": false}},
			},
			"/v1/responses": {
				Enable: map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": effortPlaceholder}},
			},
		},
	}
}

func TestApplyEffortSubstitutesMappedEffort(t *testing.T) {
	body := map[string]any{"model": "auto", "messages": []any{}}
	ApplyEffort(body, "/v1/chat/completions", Candidate{
		Model:  Model{Name: "m", Reasoning: effortReasoning()},
		Effort: "medium",
	})
	want := map[string]any{"reasoning_effort": "high", "clear_thinking": false}
	if got := body["chat_template_kwargs"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("chat_template_kwargs = %v, want %v", got, want)
	}
	if _, ok := body["messages"]; !ok {
		t.Fatal("unrelated body fields must be preserved")
	}
}

func TestApplyEffortDoesNotMutateSharedFragment(t *testing.T) {
	reasoning := effortReasoning()
	ApplyEffort(map[string]any{}, "/v1/responses", Candidate{Model: Model{Reasoning: reasoning}, Effort: "high"})
	kwargs := reasoning.Params["/v1/responses"].Enable["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != effortPlaceholder {
		t.Fatalf("catalog fragment was mutated: %v", kwargs)
	}
}

func TestApplyEffortOverridesClientReasoningFields(t *testing.T) {
	body := map[string]any{
		"chat_template_kwargs": map[string]any{"reasoning_effort": "max", "enable_thinking": true, "custom": "kept"},
	}
	ApplyEffort(body, "/v1/chat/completions", Candidate{
		Model:  Model{Reasoning: effortReasoning()},
		Effort: "low",
	})
	kwargs := body["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != "low" {
		t.Fatalf("router effort must win over client effort, got %v", kwargs["reasoning_effort"])
	}
	if kwargs["custom"] != "kept" || kwargs["enable_thinking"] != true {
		t.Fatalf("sibling kwargs must survive the merge: %v", kwargs)
	}
}

func TestApplyEffortTopLevelField(t *testing.T) {
	reasoning := &Reasoning{Params: map[string]EndpointParams{
		"/v1/chat/completions": {Enable: map[string]any{"reasoning_effort": effortPlaceholder}},
		"/v1/responses":        {Enable: map[string]any{"reasoning": map[string]any{"effort": effortPlaceholder}}},
	}}
	body := map[string]any{"reasoning_effort": "high"}
	ApplyEffort(body, "/v1/chat/completions", Candidate{Model: Model{Reasoning: reasoning}, Effort: "low"})
	if body["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v, want low", body["reasoning_effort"])
	}

	body = map[string]any{"reasoning": map[string]any{"effort": "high", "summary": "auto"}}
	ApplyEffort(body, "/v1/responses", Candidate{Model: Model{Reasoning: reasoning}, Effort: "medium"})
	want := map[string]any{"effort": "medium", "summary": "auto"}
	if !reflect.DeepEqual(body["reasoning"], want) {
		t.Fatalf("reasoning = %v, want %v", body["reasoning"], want)
	}
}

func TestApplyEffortStripsClientEffortFields(t *testing.T) {
	// A model that takes effort through chat_template_kwargs must not leave
	// the client's generic reasoning_effort / reasoning.effort behind, since
	// downstream readers (input-token counting) would otherwise apply them.
	body := map[string]any{
		"reasoning_effort": "high",
		"reasoning":        map[string]any{"effort": "high", "summary": "auto"},
	}
	ApplyEffort(body, "/v1/responses", Candidate{Model: Model{Reasoning: effortReasoning()}, Effort: "low"})
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatal("client reasoning_effort must be removed")
	}
	if !reflect.DeepEqual(body["reasoning"], map[string]any{"summary": "auto"}) {
		t.Fatalf("reasoning.effort must be removed while keeping siblings, got %v", body["reasoning"])
	}

	body = map[string]any{"reasoning": map[string]any{"effort": "high"}}
	ApplyEffort(body, "/v1/responses", Candidate{Model: Model{Reasoning: effortReasoning()}, Effort: "low"})
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("reasoning object left empty must be removed, got %v", body["reasoning"])
	}
}

func TestApplyEffortOnSubstitutesPlaceholder(t *testing.T) {
	reasoning := &Reasoning{
		EffortMap: map[string]string{"high": "max"},
		Params: map[string]EndpointParams{
			"/v1/chat/completions": {Enable: map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": effortPlaceholder}}},
		},
	}
	body := map[string]any{}
	ApplyEffort(body, "/v1/chat/completions", Candidate{Model: Model{Reasoning: reasoning}, Effort: effortOn})
	kwargs := body["chat_template_kwargs"].(map[string]any)
	if kwargs["reasoning_effort"] != "max" {
		t.Fatalf("always-on model must not leak the placeholder, got %v", kwargs["reasoning_effort"])
	}
}

func TestApplyEffortOffUsesDisableFragment(t *testing.T) {
	reasoning := &Reasoning{Params: map[string]EndpointParams{
		"/v1/chat/completions": {
			Enable:  map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}},
			Disable: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
		},
	}}
	body := map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}
	ApplyEffort(body, "/v1/chat/completions", Candidate{Model: Model{Reasoning: reasoning}, Effort: effortOff})
	kwargs := body["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != false {
		t.Fatalf("enable_thinking = %v, want false", kwargs["enable_thinking"])
	}

	body = map[string]any{}
	ApplyEffort(body, "/v1/chat/completions", Candidate{Model: Model{Reasoning: reasoning}, Effort: effortOn})
	kwargs = body["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != true {
		t.Fatalf("enable_thinking = %v, want true", kwargs["enable_thinking"])
	}
}

func TestApplyEffortWithoutParamsOnlyStripsClientEffort(t *testing.T) {
	body := map[string]any{"reasoning_effort": "high", "messages": []any{}}
	ApplyEffort(body, "/v1/chat/completions", Candidate{Model: Model{Name: "no-reasoning"}, Effort: effortOff})
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatal("client effort must be stripped even when the model has no reasoning params")
	}
	if _, ok := body["messages"]; !ok || len(body) != 1 {
		t.Fatalf("nothing else may change for a model without reasoning params: %v", body)
	}

	body = map[string]any{"messages": []any{}}
	ApplyEffort(body, "/v1/embeddings", Candidate{Model: Model{Reasoning: effortReasoning()}, Effort: "low"})
	if len(body) != 1 {
		t.Fatalf("unknown endpoint must not add fragments: %v", body)
	}
}
