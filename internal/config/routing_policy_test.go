package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The routing policy keys shipped in the earlier homelab build must keep loading unchanged.
func TestLoadConfigKeepsRoutingPolicyKeys(t *testing.T) {
	raw := `port: 8317
routing:
  strategy: round-robin
  policy:
    enabled: true
    defaults:
      route:
        - provider: Claude
          auth-order: [primary.json, " backup.json ", primary.json]
          include-remaining-auth: false
        - provider: codex
      include-remaining-providers: true
    model-overrides:
      " claude-opus-4 ":
        route:
          - provider: claude
            auth-order: [backup.json]
            include-remaining-auth: true
    fallback:
      on: [quota, "429", bogus, 5xx, network]
    observability:
      trace-limit: 5000
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	policy := cfg.Routing.Policy
	if !policy.Enabled || !policy.Defaults.IncludeRemainingProviders {
		t.Fatalf("policy flags lost: %+v", policy)
	}
	wantDefaults := []RoutingPolicyRoute{
		{Provider: "claude", AuthOrder: []string{"primary.json", "backup.json"}},
		{Provider: "codex"},
	}
	if !reflect.DeepEqual(policy.Defaults.Route, wantDefaults) {
		t.Fatalf("defaults route = %+v, want %+v", policy.Defaults.Route, wantDefaults)
	}
	override, ok := policy.ModelOverrides["claude-opus-4"]
	if !ok || !override.Route[0].IncludeRemainingAuth || override.Route[0].AuthOrder[0] != "backup.json" {
		t.Fatalf("model override = %+v, ok=%v", override, ok)
	}
	wantTriggers := []string{"exhausted", "rate_limited", "server_error", "transport_error"}
	if !reflect.DeepEqual(policy.Fallback.On, wantTriggers) {
		t.Fatalf("fallback.on = %v, want %v", policy.Fallback.On, wantTriggers)
	}
	if policy.Observability.TraceLimit != MaxRoutingTraceLimit {
		t.Fatalf("trace-limit = %d, want clamp to %d", policy.Observability.TraceLimit, MaxRoutingTraceLimit)
	}
}

func TestSanitizeRoutingPolicyLeavesDisabledPolicyEmpty(t *testing.T) {
	cfg := &Config{}
	cfg.SanitizeRoutingPolicy()
	if cfg.Routing.Policy.Fallback.On != nil {
		t.Fatalf("disabled policy gained triggers: %v", cfg.Routing.Policy.Fallback.On)
	}
}
