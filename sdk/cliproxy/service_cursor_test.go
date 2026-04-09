package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

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

func TestOpenAICompatInfoFromAuth_CursorProviderIsNotTreatedAsCompat(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "cursor",
		Attributes: map[string]string{
			"provider_key": "cursor",
			"compat_name":  "cursor",
		},
	}
	providerKey, compatName, ok := openAICompatInfoFromAuth(auth)
	if ok || providerKey != "" || compatName != "" {
		t.Fatalf("expected cursor auth to stay native, got provider=%q compat=%q ok=%v", providerKey, compatName, ok)
	}
}

func TestForceModelPrefixForProvider_CursorAlwaysForced(t *testing.T) {
	if !forceModelPrefixForProvider("cursor", false) {
		t.Fatal("expected cursor model prefix forcing to be enabled")
	}
	if forceModelPrefixForProvider("codex", false) {
		t.Fatal("expected non-cursor providers to follow global setting")
	}
	if !forceModelPrefixForProvider("codex", true) {
		t.Fatal("expected global force flag to force all providers")
	}
}
