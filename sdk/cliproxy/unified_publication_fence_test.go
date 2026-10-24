package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func unifiedAliasTestConfig(enabled bool) *sdkconfig.Config {
	return &sdkconfig.Config{Routing: config.RoutingConfig{UnifiedModels: config.UnifiedModels{Enabled: enabled, Models: []config.UnifiedModel{{
		ID:     "unified-test",
		Routes: []config.UnifiedModelRoute{{Provider: "claude", Source: "source-model", Model: "wire", Plan: "included"}},
	}}}}}
}

func unifiedAliasTestService(t *testing.T, cfg *sdkconfig.Config, subscriptionActive bool) (*Service, *coreauth.Auth) {
	t.Helper()
	service := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil), pluginHost: pluginhost.New()}
	auth, err := service.coreManager.Register(t.Context(), &coreauth.Auth{
		ID: t.Name(), Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata: map[string]any{"is_subs_active": subscriptionActive},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	return service, auth
}

// The alias comes from the config captured with the fence, and only that config is consulted.
func TestUnifiedAliasPublicationUsesCapturedConfig(t *testing.T) {
	cfg := unifiedAliasTestConfig(true)
	service, auth := unifiedAliasTestService(t, cfg, false)
	fence := newModelRegistrationFence(auth, cfg)
	if !service.registerResolvedModelsForAuthFenced(auth, fence, "claude", []*ModelInfo{{ID: "source-model"}}) {
		t.Fatal("publication rejected")
	}
	reg := GlobalModelRegistry()
	if !reg.ClientSupportsModel(auth.ID, cfg.Routing.UnifiedModels.PublicID("unified-test")) {
		t.Fatal("alias missing from the captured config")
	}
}

// A unified-model config change between capture and publication supersedes the computation.
func TestUnifiedAliasPublicationRejectedAfterConfigChange(t *testing.T) {
	cfg := unifiedAliasTestConfig(true)
	service, auth := unifiedAliasTestService(t, cfg, false)
	fence := newModelRegistrationFence(auth, cfg)
	service.cfgMu.Lock()
	service.cfg = unifiedAliasTestConfig(false)
	service.cfgMu.Unlock()
	if service.registerResolvedModelsForAuthFenced(auth, fence, "claude", []*ModelInfo{{ID: "source-model"}}) {
		t.Fatal("stale unified publication was accepted")
	}
	if GlobalModelRegistry().ClientSupportsModel(auth.ID, "source-model") {
		t.Fatal("stale publication reached the registry")
	}
}

// A subscription change between capture and publication supersedes the computation.
func TestUnifiedAliasPublicationRejectedAfterSubscriptionChange(t *testing.T) {
	cfg := unifiedAliasTestConfig(true)
	service, auth := unifiedAliasTestService(t, cfg, false)
	fence := newModelRegistrationFence(auth, cfg)
	updated := auth.Clone()
	updated.Metadata = map[string]any{"is_subs_active": true}
	if _, err := service.coreManager.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	if service.registerResolvedModelsForAuthFenced(auth, fence, "claude", []*ModelInfo{{ID: "source-model"}}) {
		t.Fatal("publication computed from an older subscription state was accepted")
	}
}
