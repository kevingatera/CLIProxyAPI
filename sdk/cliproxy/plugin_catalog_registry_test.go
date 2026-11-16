package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// A static-scope executor plugin publishes its catalog as a global client,
// outside per-auth registration. That publication must honour the live global
// list, and a reload must republish it in both directions.
func TestPluginCatalogRegistryAppliesGlobalExclusionsAcrossReload(t *testing.T) {
	const provider = "catalog-exclusion-fixture"
	pluginID := t.Name()
	plugin := issue7512Plugin{
		provider: provider,
		models:   []pluginapi.ModelInfo{{ID: "demo-a"}, {ID: "Demo-Blocked"}},
	}
	host := pluginhost.New()
	host.RegisterPluginForTest(pluginID, pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
		ModelProvider:      plugin,
		Executor:           plugin,
		ExecutorModelScope: pluginapi.ExecutorModelScopeStatic,
	}})
	clientID := "plugin:" + pluginID + ":" + provider + ":executor"
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	service := &Service{cfg: &config.Config{}, pluginHost: host}
	publish := func() {
		host.RegisterModels(t.Context(), service.pluginCatalogRegistry())
		host.RegisterExecutors(coreauth.NewManager(nil, nil, nil), service.pluginCatalogRegistry())
	}

	// Mixed-case pattern: matching must not depend on the stored case.
	if commit := service.commitConfigUpdate(&config.Config{GlobalExcludedModels: []string{"DEMO-BLOCKED"}}); commit.cfg == nil {
		t.Fatal("config update was rejected")
	}
	publish()
	if !reg.ClientSupportsModel(clientID, "demo-a") {
		t.Fatal("demo-a was not published")
	}
	if reg.ClientSupportsModel(clientID, "Demo-Blocked") || reg.ClientSupportsModel(clientID, "demo-blocked") {
		t.Fatal("globally excluded static plugin model was published")
	}

	// Removing the exclusion on reload must republish the model.
	if commit := service.commitConfigUpdate(&config.Config{}); commit.cfg == nil {
		t.Fatal("config update was rejected")
	}
	publish()
	if !reg.ClientSupportsModel(clientID, "Demo-Blocked") {
		t.Fatal("model is no longer excluded but was not republished after reload")
	}
}
