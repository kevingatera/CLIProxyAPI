package registry

import "testing"

func TestCodexTerraSurvivesCatalogRefresh(t *testing.T) {
	for name, models := range map[string][]*ModelInfo{
		"team":         GetCodexTeamModels(),
		"plus":         GetCodexPlusModels(),
		"pro":          GetCodexProModels(),
		"stale remote": withCodexTerra([]*ModelInfo{{ID: "gpt-6.1-sol"}}),
	} {
		t.Run(name, func(t *testing.T) {
			var terra *ModelInfo
			for _, model := range models {
				if model.ID == "gpt-6.1-terra" {
					if terra != nil {
						t.Fatal("duplicate Terra registration")
					}
					terra = model
				}
			}
			if terra == nil || terra.ContextLength != 500000 || terra.MaxCompletionTokens != 0 {
				t.Fatalf("missing native Terra metadata: %+v", terra)
			}
		})
	}
	upstream := &ModelInfo{ID: "gpt-6.1-terra", ContextLength: 872000}
	models := withCodexTerra([]*ModelInfo{upstream})
	if len(models) != 1 || models[0] != upstream {
		t.Fatal("existing upstream metadata must take precedence")
	}
}
