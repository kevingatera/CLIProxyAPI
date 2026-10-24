package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// global-excluded-models covers every auth kind, including API keys and
// credentials with their own excluded_models, unlike oauth-excluded-models.
func TestModelRegistrationRejectsObsoleteGlobalExcludedModels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string]string
	}{
		{"oauth", map[string]string{"auth_kind": "oauth"}},
		{"api key", map[string]string{"auth_kind": "apikey"}},
		{"credential override", map[string]string{"auth_kind": "oauth", "excluded_models": "own-*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldCfg := &config.Config{GlobalExcludedModels: []string{"model-a"}}
			newCfg := &config.Config{GlobalExcludedModels: []string{"model-b"}}
			service := &Service{cfg: oldCfg, coreManager: coreauth.NewManager(nil, nil, nil)}
			auth, errRegister := service.coreManager.Register(t.Context(), &coreauth.Auth{ID: t.Name(), Provider: "claude", Attributes: tc.attrs})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := GlobalModelRegistry()
			reg.RegisterClient(auth.ID, "claude", []*ModelInfo{{ID: "model-old"}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			epoch := reg.ClientRegistrationEpoch(auth.ID)
			fence := newModelRegistrationFence(auth, oldCfg)

			// The captured list filters, even after the live config changes.
			if commit := service.commitConfigUpdate(newCfg); commit.cfg != newCfg {
				t.Fatal("config update was rejected")
			}
			if service.registerResolvedModelsForAuthFenced(auth, fence, "claude", []*ModelInfo{{ID: "model-a"}, {ID: "model-b"}}) {
				t.Fatal("result computed under obsolete global exclusions was published")
			}
			if got := reg.ClientRegistrationEpoch(auth.ID); got != epoch {
				t.Fatalf("obsolete result advanced epoch from %d to %d", epoch, got)
			}

			current := newModelRegistrationFence(auth, newCfg)
			if !service.registerResolvedModelsForAuthFenced(auth, current, "claude", []*ModelInfo{{ID: "model-a"}, {ID: "model-b"}}) {
				t.Fatal("current global exclusions could not publish")
			}
			if !reg.ClientSupportsModel(auth.ID, "model-a") || reg.ClientSupportsModel(auth.ID, "model-b") {
				t.Fatalf("models = %v, want only model-a", reg.GetModelsForClient(auth.ID))
			}
		})
	}
}

// Plugin registration shares the final publication path, so a plugin result
// computed under an obsolete global list must be discarded and recomputed.
func TestPluginModelRegistrationRejectsObsoleteGlobalExcludedModels(t *testing.T) {
	const provider = "global-exclusion-fixture"
	oldCfg := &config.Config{GlobalExcludedModels: []string{"model-a"}}
	newCfg := &config.Config{GlobalExcludedModels: []string{"model-b"}}
	manager := coreauth.NewManager(nil, nil, nil)
	host := pluginhost.New()
	service := &Service{cfg: oldCfg, coreManager: manager, pluginHost: host}
	calls := 0
	modelProvider := &exclusionFenceModelProvider{provider: provider, onModels: func() pluginapi.ModelResponse {
		calls++
		if calls == 1 {
			if commit := service.commitConfigUpdate(newCfg); commit.cfg != newCfg {
				t.Fatal("config update was rejected")
			}
		}
		return pluginapi.ModelResponse{Provider: provider, Models: []pluginapi.ModelInfo{{ID: "model-a"}, {ID: "model-b"}}}
	}}
	pluginID := t.Name()
	host.RegisterPluginForTest(pluginID, pluginapi.Plugin{Capabilities: pluginapi.Capabilities{ModelProvider: modelProvider}})
	host.RegisterModels(t.Context(), registry.GetGlobalRegistry())
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient("plugin:" + pluginID + ":" + provider) })
	registered, errRegister := manager.Register(t.Context(), &coreauth.Auth{ID: t.Name(), Provider: provider, Attributes: map[string]string{"auth_kind": "apikey"}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	reg.RegisterClient(registered.ID, provider, []*ModelInfo{{ID: "model-old"}})
	t.Cleanup(func() { reg.UnregisterClient(registered.ID) })
	epoch := reg.ClientRegistrationEpoch(registered.ID)

	service.registerModelsForAuth(t.Context(), registered)
	if calls != 1 {
		t.Fatalf("plugin calls = %d, want 1", calls)
	}
	if got := reg.ClientRegistrationEpoch(registered.ID); got != epoch {
		t.Fatalf("obsolete plugin result advanced epoch from %d to %d", epoch, got)
	}
	service.registerModelsForAuth(t.Context(), registered)
	if !reg.ClientSupportsModel(registered.ID, "model-a") || reg.ClientSupportsModel(registered.ID, "model-b") {
		t.Fatalf("final plugin models = %v, want only model-a", reg.GetModelsForClient(registered.ID))
	}
}
