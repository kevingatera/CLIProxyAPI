package registry

import "testing"

func TestCodexTeamModelsIncludeGPT54(t *testing.T) {
	models := GetCodexTeamModels()
	if !containsModelID(models, "gpt-5.4") {
		t.Fatal("expected codex team models to include gpt-5.4")
	}
}

func TestCodexPlusModelsIncludeGPT54(t *testing.T) {
	models := GetCodexPlusModels()
	if !containsModelID(models, "gpt-5.4") {
		t.Fatal("expected codex plus models to include gpt-5.4")
	}
}

func TestCodexProModelsIncludeGPT54(t *testing.T) {
	models := GetCodexProModels()
	if !containsModelID(models, "gpt-5.4") {
		t.Fatal("expected codex pro models to include gpt-5.4")
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
