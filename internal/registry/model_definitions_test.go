package registry

import "testing"

func TestCodexTierModelsIncludeMini(t *testing.T) {
	tests := []struct {
		name   string
		models []*ModelInfo
	}{
		{name: "free", models: GetCodexFreeModels()},
		{name: "team", models: GetCodexTeamModels()},
		{name: "plus", models: GetCodexPlusModels()},
		{name: "pro", models: GetCodexProModels()},
	}

	for _, tc := range tests {
		if !containsModelID(tc.models, "gpt-5.4-mini") {
			t.Fatalf("expected codex %s models to include gpt-5.4-mini", tc.name)
		}
	}
}

func TestCodexPaidTiersIncludeGPT54(t *testing.T) {
	tests := []struct {
		name   string
		models []*ModelInfo
	}{
		{name: "team", models: GetCodexTeamModels()},
		{name: "plus", models: GetCodexPlusModels()},
		{name: "pro", models: GetCodexProModels()},
	}

	for _, tc := range tests {
		if !containsModelID(tc.models, "gpt-5.4") {
			t.Fatalf("expected codex %s models to include gpt-5.4", tc.name)
		}
	}
}

func TestCodexPaidTiersIncludeGPT55(t *testing.T) {
	tests := []struct {
		name   string
		models []*ModelInfo
	}{
		{name: "team", models: GetCodexTeamModels()},
		{name: "plus", models: GetCodexPlusModels()},
		{name: "pro", models: GetCodexProModels()},
	}

	for _, tc := range tests {
		if !containsModelID(tc.models, "gpt-5.5") {
			t.Fatalf("expected codex %s models to include gpt-5.5", tc.name)
		}
	}
}

func containsModelID(models []*ModelInfo, target string) bool {
	for _, model := range models {
		if model != nil && model.ID == target {
			return true
		}
	}
	return false
}
