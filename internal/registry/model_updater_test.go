package registry

import "testing"

func TestValidateModelsCatalogAllowsOptionalProviderSectionsToBeEmpty(t *testing.T) {
	data := cloneStaticModelsForTest(getModels())
	data.Qwen = nil
	data.IFlow = nil

	if err := validateModelsCatalog(data); err != nil {
		t.Fatalf("validateModelsCatalog() returned error for optional empty sections: %v", err)
	}
}

func TestFillOptionalModelSectionsPreservesFallbackCatalog(t *testing.T) {
	fallback := cloneStaticModelsForTest(getModels())
	data := cloneStaticModelsForTest(fallback)
	data.Qwen = nil
	data.IFlow = nil

	fillOptionalModelSections(data, fallback)

	if len(data.Qwen) == 0 {
		t.Fatalf("expected qwen models to be filled from fallback")
	}
	if len(data.IFlow) == 0 {
		t.Fatalf("expected iflow models to be filled from fallback")
	}

	data.Qwen[0].ID = "mutated"
	if fallback.Qwen[0].ID == "mutated" {
		t.Fatalf("expected optional fill to clone fallback models")
	}
}

func cloneStaticModelsForTest(in *staticModelsJSON) *staticModelsJSON {
	if in == nil {
		return nil
	}
	return &staticModelsJSON{
		Claude:      cloneModelInfos(in.Claude),
		Gemini:      cloneModelInfos(in.Gemini),
		Vertex:      cloneModelInfos(in.Vertex),
		GeminiCLI:   cloneModelInfos(in.GeminiCLI),
		AIStudio:    cloneModelInfos(in.AIStudio),
		CodexFree:   cloneModelInfos(in.CodexFree),
		CodexTeam:   cloneModelInfos(in.CodexTeam),
		CodexPlus:   cloneModelInfos(in.CodexPlus),
		CodexPro:    cloneModelInfos(in.CodexPro),
		Qwen:        cloneModelInfos(in.Qwen),
		IFlow:       cloneModelInfos(in.IFlow),
		Kimi:        cloneModelInfos(in.Kimi),
		Antigravity: cloneModelInfos(in.Antigravity),
	}
}
