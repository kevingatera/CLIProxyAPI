package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	executor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestUnifiedSubscriptionRouteRequiresActiveOAuth(t *testing.T) {
	route := config.UnifiedModelRoute{Provider: "meta", AuthKind: "oauth", SubscriptionOnly: true, Source: "muse-spark-test", Model: "muse-spark-test", Plan: "included"}
	for _, test := range []struct {
		kind   string
		active any
		want   bool
	}{
		{"oauth", true, true}, {"oauth", false, false}, {"oauth", nil, false}, {"oauth", "true", false}, {"apikey", true, false},
	} {
		a := &Auth{Provider: "meta", Attributes: map[string]string{"auth_kind": test.kind}, Metadata: map[string]any{"is_subs_active": test.active}}
		if got := MatchesUnifiedRoute(route, a); got != test.want {
			t.Fatalf("kind=%s active=%v got=%v", test.kind, test.active, got)
		}
	}
}

func TestUnifiedSubscriptionExpiryCannotExecuteCachedModel(t *testing.T) {
	m := NewManager(nil, nil, nil)
	e := &unifiedTestExecutor{routingPolicyTestExecutor: routingPolicyTestExecutor{id: "meta"}}
	m.RegisterExecutor(e)
	cfg := &config.Config{}
	cfg.Routing.UnifiedModels = config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{{ID: "muse-spark-test", Routes: []config.UnifiedModelRoute{{Provider: "meta", AuthKind: "oauth", SubscriptionOnly: true, Source: "muse-spark-test", Model: "muse-spark-test", Plan: "included"}}}}}
	m.SetConfig(cfg)
	a := &Auth{ID: "synthetic-meta-subscription-expired", Provider: "meta", Attributes: map[string]string{"auth_kind": "oauth"}, Metadata: map[string]any{"is_subs_active": false}}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, "meta", []*registry.ModelInfo{{ID: "muse-spark-test"}, {ID: "cliproxy/muse-spark-test"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	_, err := m.Execute(context.Background(), []string{"meta"}, executor.Request{Model: "cliproxy/muse-spark-test"}, executor.Options{})
	if err == nil || len(e.models) != 0 {
		t.Fatal("inactive subscription reached inference")
	}
}
