package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaAwareRoutingStrategyAliases(t *testing.T) {
	for _, name := range []string{"quota-aware", "QuotaAware", " quota ", "qa"} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: name},
		})
		if state.strategy != "quota-aware" {
			t.Fatalf("strategy(%q) = %q, want quota-aware", name, state.strategy)
		}
		selector := newRoutingSelector(state)
		quota, ok := selector.(*coreauth.QuotaAwareSelector)
		if !ok {
			t.Fatalf("selector type = %T, want *auth.QuotaAwareSelector", selector)
		}
		quota.Stop()
	}
}

// A global proxy-url change rebuilds the selector only for quota-aware routing, whose
// probes use it. Other strategies keep their selector state and affinity bindings.
func TestProxyURLOnlyChangesQuotaAwareRoutingState(t *testing.T) {
	state := func(strategy, proxy string) routingRuntimeState {
		return normalizedRoutingRuntimeState(&internalconfig.Config{
			SDKConfig: internalconfig.SDKConfig{ProxyURL: proxy},
			Routing:   internalconfig.RoutingConfig{Strategy: strategy},
		})
	}
	for _, strategy := range []string{"round-robin", "weighted-round-robin", "fill-first"} {
		if state(strategy, "http://proxy-a.example:3128") != state(strategy, "http://proxy-b.example:3128") {
			t.Fatalf("strategy %s: proxy-url change alters routing identity", strategy)
		}
	}
	if state("quota-aware", "http://proxy-a.example:3128") == state("quota-aware", "http://proxy-b.example:3128") {
		t.Fatal("quota-aware: proxy-url change must rebuild the selector")
	}
}
