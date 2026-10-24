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
