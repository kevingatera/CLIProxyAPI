package cliproxy

import "testing"

func TestCursorModelsFromAuthMetadata_UsesMetadataModels(t *testing.T) {
	models := cursorModelsFromAuthMetadata(map[string]any{
		"models": []any{"auto", map[string]any{"id": "gpt-5.4-medium"}, "auto"},
	})
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "auto" || models[1].ID != "gpt-5.4-medium" {
		t.Fatalf("unexpected model ids: %s, %s", models[0].ID, models[1].ID)
	}
}

func TestCursorModelsFromAuthMetadata_FallsBackToDefaults(t *testing.T) {
	models := cursorModelsFromAuthMetadata(nil)
	if len(models) == 0 {
		t.Fatal("expected default cursor models")
	}
	foundAuto := false
	for _, model := range models {
		if model != nil && model.ID == "auto" {
			foundAuto = true
			break
		}
	}
	if !foundAuto {
		t.Fatal("expected default cursor models to include auto")
	}
}
