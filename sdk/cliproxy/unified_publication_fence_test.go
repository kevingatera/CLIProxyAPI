package cliproxy

import (
	"testing"
	"time"

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

// A disabled credential has already left unified selection, so a change to any
// fence input must not strand its models in the registry: the removal still has
// to succeed. A removal captured before the credential is re-enabled is stale
// and must be rejected, before and after the re-enabled credential republishes.
func TestUnifiedDisabledAuthRemovalSurvivesFenceInputChangeAndRejectsStaleRemoval(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, manager *coreauth.Manager, disabled *coreauth.Auth, next *sdkconfig.Config) *coreauth.Auth
	}{
		{
			name: "unified configuration",
			mutate: func(_ *testing.T, _ *coreauth.Manager, disabled *coreauth.Auth, next *sdkconfig.Config) *coreauth.Auth {
				next.Routing.UnifiedModels.Models[0].Routes[0].Model = "new-wire"
				return disabled
			},
		},
		{
			name: "subscription state",
			mutate: func(t *testing.T, manager *coreauth.Manager, disabled *coreauth.Auth, _ *sdkconfig.Config) *coreauth.Auth {
				t.Helper()
				updated := disabled.Clone()
				updated.Metadata = map[string]any{"is_subs_active": true}
				updated, err := manager.Update(t.Context(), updated)
				if err != nil {
					t.Fatal(err)
				}
				return updated
			},
		},
		{
			name: "global exclusions",
			mutate: func(_ *testing.T, _ *coreauth.Manager, disabled *coreauth.Auth, next *sdkconfig.Config) *coreauth.Auth {
				next.GlobalExcludedModels = []string{"unrelated-model"}
				return disabled
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := unifiedAliasTestConfig(true)
			service, auth := unifiedAliasTestService(t, cfg, false)
			manager := service.coreManager
			manager.SetConfig(cfg)
			manager.RegisterExecutor(mockBatchTestExecutor{provider: "claude"})
			reg := GlobalModelRegistry()
			alias := cfg.Routing.UnifiedModels.PublicID("unified-test")
			assertSelectable := func() {
				t.Helper()
				if !reg.ClientSupportsModel(auth.ID, "source-model") || !reg.ClientSupportsModel(auth.ID, alias) {
					t.Fatal("source model or unified alias missing")
				}
				routes := manager.UnifiedRoutes(alias, time.Now())
				if len(routes) != 1 || routes[0].AuthID != auth.ID || !routes[0].Eligible {
					t.Fatalf("unified routes = %+v, want the enabled credential", routes)
				}
			}
			if !service.registerResolvedModelsForAuthFenced(auth, newModelRegistrationFence(auth, cfg), "claude", []*ModelInfo{{ID: "source-model"}}) {
				t.Fatal("initial publication rejected")
			}
			assertSelectable()

			disabled := auth.Clone()
			disabled.Disabled = true
			disabled.Status = coreauth.StatusDisabled
			disabled, err := manager.Update(t.Context(), disabled)
			if err != nil {
				t.Fatal(err)
			}
			removal := newModelRegistrationFence(disabled, cfg)

			next := unifiedAliasTestConfig(true)
			disabled = tc.mutate(t, manager, disabled, next)
			service.cfgMu.Lock()
			service.cfg = next
			service.cfgMu.Unlock()
			manager.SetConfig(next)
			staleRemoval := newModelRegistrationFence(disabled, next)

			if !service.unregisterModelsForAuthFenced(disabled, removal) {
				t.Fatal("fence input change rejected the current disabled credential's removal")
			}
			if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
				t.Fatalf("disabled credential retained models: %v", models)
			}
			if routes := manager.UnifiedRoutes(alias, time.Now()); len(routes) != 0 {
				t.Fatalf("disabled credential remains in unified selection: %+v", routes)
			}

			enabled := disabled.Clone()
			enabled.Disabled = false
			enabled.Status = coreauth.StatusActive
			enabled, err = manager.Update(t.Context(), enabled)
			if err != nil {
				t.Fatal(err)
			}
			// No registry publication has advanced the epoch yet; the auth fence alone
			// must reject a removal captured while the credential was still disabled.
			if service.unregisterModelsForAuthFenced(disabled, staleRemoval) {
				t.Fatal("stale disabled removal accepted after re-enable")
			}
			if !service.registerResolvedModelsForAuthFenced(enabled, newModelRegistrationFence(enabled, next), "claude", []*ModelInfo{{ID: "source-model"}}) {
				t.Fatal("re-enabled publication rejected")
			}
			epoch := reg.ClientRegistrationEpoch(auth.ID)
			if service.unregisterModelsForAuthFenced(disabled, staleRemoval) {
				t.Fatal("stale disabled removal accepted after re-publication")
			}
			if reg.ClientRegistrationEpoch(auth.ID) != epoch {
				t.Fatal("stale removal changed the registry epoch")
			}
			assertSelectable()
		})
	}
}
