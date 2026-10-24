package registry

import (
	"strings"
	"testing"
)

func kimiUpstreamK3() *ModelInfo {
	return &ModelInfo{
		ID:                       "kimi-k3",
		Object:                   "model",
		OwnedBy:                  "moonshot",
		Type:                     "kimi",
		Description:              "upstream description",
		ContextLength:            1048576,
		MaxCompletionTokens:      65536,
		SupportedInputModalities: []string{"text", "image", "video"},
		Thinking:                 &ThinkingSupport{ZeroAllowed: true, Levels: []string{"low", "high", "max"}},
	}
}

func boolPtr(v bool) *bool { return &v }

func intPtr(v int) *int { return &v }

func TestApplyLocalModelPatchesMergesFieldsOntoUpstreamEntry(t *testing.T) {
	catalog := &staticModelsJSON{Kimi: []*ModelInfo{kimiUpstreamK3()}}
	patches := &localModelPatchFile{Kimi: []localModelPatch{{
		ID:            "kimi-k3",
		ContextLength: intPtr(262144),
		Thinking:      &localThinkingPatch{ZeroAllowed: boolPtr(false)},
	}}}

	if err := applyLocalModelPatches(catalog, patches); err != nil {
		t.Fatalf("applyLocalModelPatches: %v", err)
	}
	got := catalog.Kimi[0]
	if got.ContextLength != 262144 {
		t.Fatalf("context_length = %d, want 262144", got.ContextLength)
	}
	if got.Thinking == nil || got.Thinking.ZeroAllowed {
		t.Fatalf("thinking.zero_allowed must be explicit false, got %+v", got.Thinking)
	}
	if strings.Join(got.Thinking.Levels, ",") != "low,high,max" {
		t.Fatalf("thinking levels changed: %v", got.Thinking.Levels)
	}
	if got.Description != "upstream description" || got.MaxCompletionTokens != 65536 || len(got.SupportedInputModalities) != 3 {
		t.Fatalf("unpatched upstream fields changed: %+v", got)
	}
}

func TestApplyLocalModelPatchesLeavesAbsentFieldsAlone(t *testing.T) {
	catalog := &staticModelsJSON{Kimi: []*ModelInfo{kimiUpstreamK3()}}
	patches := &localModelPatchFile{Kimi: []localModelPatch{{
		ID:            "kimi-k3",
		ContextLength: intPtr(262144),
	}}}

	if err := applyLocalModelPatches(catalog, patches); err != nil {
		t.Fatalf("applyLocalModelPatches: %v", err)
	}
	if got := catalog.Kimi[0]; got.Thinking == nil || !got.Thinking.ZeroAllowed {
		t.Fatalf("absent zero_allowed must keep upstream true, got %+v", got.Thinking)
	}
}

func TestApplyLocalModelPatchesDoesNotMutateUpstreamEntries(t *testing.T) {
	upstream := kimiUpstreamK3()
	catalog := &staticModelsJSON{Kimi: []*ModelInfo{upstream}}
	patches := &localModelPatchFile{Kimi: []localModelPatch{{
		ID:            "kimi-k3",
		ContextLength: intPtr(262144),
		Thinking:      &localThinkingPatch{ZeroAllowed: boolPtr(false)},
	}}}

	if err := applyLocalModelPatches(catalog, patches); err != nil {
		t.Fatalf("applyLocalModelPatches: %v", err)
	}
	if upstream.ContextLength != 1048576 || !upstream.Thinking.ZeroAllowed {
		t.Fatalf("patch mutated the source entry: %+v thinking=%+v", upstream, upstream.Thinking)
	}
}

func TestApplyLocalModelPatchesRejectsBadPatchAndKeepsCatalog(t *testing.T) {
	cases := map[string]localModelPatchFile{
		"duplicate id": {Kimi: []localModelPatch{
			{ID: "kimi-k3", ContextLength: intPtr(262144)},
			{ID: "kimi-k3", ContextLength: intPtr(131072)},
		}},
		"non-positive context": {Kimi: []localModelPatch{
			{ID: "kimi-k3", ContextLength: intPtr(0)},
		}},
		"thinking without upstream metadata": {Kimi: []localModelPatch{
			{ID: "kimi-plain", Thinking: &localThinkingPatch{ZeroAllowed: boolPtr(false)}},
		}},
	}
	for name, patches := range cases {
		t.Run(name, func(t *testing.T) {
			plain := &ModelInfo{ID: "kimi-plain", Type: "kimi", ContextLength: 131072}
			catalog := &staticModelsJSON{Kimi: []*ModelInfo{kimiUpstreamK3(), plain}}
			if err := applyLocalModelPatches(catalog, &patches); err == nil {
				t.Fatal("expected error")
			}
			if catalog.Kimi[0].ContextLength != 1048576 || !catalog.Kimi[0].Thinking.ZeroAllowed {
				t.Fatalf("failed apply changed the catalog: %+v", catalog.Kimi[0])
			}
		})
	}
}

func TestApplyLocalModelPatchesSkipsUnknownIDs(t *testing.T) {
	catalog := &staticModelsJSON{Kimi: []*ModelInfo{kimiUpstreamK3()}}
	patches := &localModelPatchFile{Kimi: []localModelPatch{
		{ID: "kimi-removed-upstream", ContextLength: intPtr(262144)},
		{ID: "kimi-k3", Thinking: &localThinkingPatch{ZeroAllowed: boolPtr(false)}},
	}}

	if err := applyLocalModelPatches(catalog, patches); err != nil {
		t.Fatalf("applyLocalModelPatches: %v", err)
	}
	if got := catalog.Kimi[0]; got.Thinking.ZeroAllowed || got.ContextLength != 1048576 {
		t.Fatalf("known patch not applied alongside unknown id: %+v", got)
	}
}

func TestApplyLocalModelPatchesIsScopedToKimiSection(t *testing.T) {
	claudeSameID := &ModelInfo{ID: "kimi-k3", Type: "claude", ContextLength: 1048576}
	catalog := &staticModelsJSON{
		Claude: []*ModelInfo{claudeSameID},
		Kimi:   []*ModelInfo{kimiUpstreamK3()},
	}
	patches := &localModelPatchFile{Kimi: []localModelPatch{{
		ID:            "kimi-k3",
		ContextLength: intPtr(262144),
	}}}

	if err := applyLocalModelPatches(catalog, patches); err != nil {
		t.Fatalf("applyLocalModelPatches: %v", err)
	}
	if catalog.Claude[0].ContextLength != 1048576 {
		t.Fatalf("kimi patch leaked into claude section: %+v", catalog.Claude[0])
	}
}

func TestEmbeddedLocalModelPatchesApplyToLoadedCatalog(t *testing.T) {
	patches, err := loadLocalModelPatches()
	if err != nil {
		t.Fatalf("loadLocalModelPatches: %v", err)
	}
	if len(patches.Kimi) != 4 {
		t.Fatalf("embedded kimi patches = %d, want 4", len(patches.Kimi))
	}

	models := getModels()
	if models == nil {
		t.Fatal("catalog not loaded")
	}
	byID := make(map[string]*ModelInfo, len(models.Kimi))
	for _, model := range models.Kimi {
		byID[model.ID] = model
	}
	k3 := byID["kimi-k3"]
	if k3 == nil || k3.ContextLength != 262144 || k3.Thinking == nil || k3.Thinking.ZeroAllowed {
		t.Fatalf("kimi-k3 patch not applied on load: %+v", k3)
	}
	if k3k := byID["kimi-k3-256k"]; k3k == nil || k3k.ContextLength != 262144 || k3k.Thinking.ZeroAllowed {
		t.Fatalf("kimi-k3-256k patch not applied on load: %+v", k3k)
	}
	if k28 := byID["kimi-k2.8"]; k28 == nil || k28.ContextLength != 1048576 || k28.Thinking.ZeroAllowed {
		t.Fatalf("kimi-k2.8 patch not applied on load: %+v", k28)
	}
}

func TestPublishCatalogBytesReappliesLocalPatches(t *testing.T) {
	oldModels := getModels()
	defer func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = oldModels
		modelsCatalogStore.mu.Unlock()
	}()

	remote := []byte(`{"kimi":[{"id":"kimi-k3","context_length":1048576,"thinking":{"zero_allowed":true,"levels":["low","high","max"]}}]}`)
	if _, err := publishCatalogBytes(remote); err != nil {
		t.Fatalf("publishCatalogBytes: %v", err)
	}
	got := getModels().Kimi
	if len(got) != 1 || got[0].ContextLength != 262144 || got[0].Thinking == nil || got[0].Thinking.ZeroAllowed {
		t.Fatalf("remote refresh dropped local patches: %+v", got)
	}
}
