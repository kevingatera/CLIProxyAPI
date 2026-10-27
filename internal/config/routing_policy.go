package config

import "strings"

// Routing fallback trigger names.
const (
	RoutingFallbackExhausted      = "exhausted"
	RoutingFallbackRateLimited    = "rate_limited"
	RoutingFallbackServerError    = "server_error"
	RoutingFallbackTransportError = "transport_error"

	// MaxRoutingTraceLimit caps routing.policy.observability.trace-limit.
	MaxRoutingTraceLimit = 2000
)

// DefaultRoutingFallbackTriggers is used when an enabled policy lists no triggers.
func DefaultRoutingFallbackTriggers() []string {
	return []string{RoutingFallbackExhausted, RoutingFallbackRateLimited, RoutingFallbackServerError, RoutingFallbackTransportError}
}

// NormalizeRoutingFallbackTrigger maps a trigger name or alias to its canonical name.
func NormalizeRoutingFallbackTrigger(trigger string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(trigger)) {
	case "exhausted", "quota", "quota_exhausted":
		return RoutingFallbackExhausted, true
	case "rate_limited", "ratelimited", "rate-limited", "429":
		return RoutingFallbackRateLimited, true
	case "server_error", "server", "5xx":
		return RoutingFallbackServerError, true
	case "transport_error", "transport", "network", "timeout":
		return RoutingFallbackTransportError, true
	default:
		return "", false
	}
}

// SanitizeRoutingPolicy normalizes routing policy fields and drops invalid entries.
func (cfg *Config) SanitizeRoutingPolicy() {
	if cfg == nil {
		return
	}
	policy := &cfg.Routing.Policy
	policy.Defaults = sanitizeRoutingPolicyRule(policy.Defaults)
	if len(policy.ModelOverrides) > 0 {
		overrides := make(map[string]RoutingPolicyRule, len(policy.ModelOverrides))
		for modelID, rule := range policy.ModelOverrides {
			if modelID = strings.TrimSpace(modelID); modelID != "" {
				overrides[modelID] = sanitizeRoutingPolicyRule(rule)
			}
		}
		policy.ModelOverrides = overrides
	}

	seen := make(map[string]struct{}, len(policy.Fallback.On))
	triggers := make([]string, 0, len(policy.Fallback.On))
	for _, trigger := range policy.Fallback.On {
		normalized, ok := NormalizeRoutingFallbackTrigger(trigger)
		if !ok {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		triggers = append(triggers, normalized)
	}
	// Defaults are materialized only for an enabled policy so a disabled policy round-trips unchanged.
	if len(triggers) == 0 && policy.Enabled {
		triggers = DefaultRoutingFallbackTriggers()
	}
	if len(triggers) > 0 {
		policy.Fallback.On = triggers
	} else {
		policy.Fallback.On = nil
	}

	if policy.Observability.TraceLimit < 0 {
		policy.Observability.TraceLimit = 0
	}
	if policy.Observability.TraceLimit > MaxRoutingTraceLimit {
		policy.Observability.TraceLimit = MaxRoutingTraceLimit
	}
}

func sanitizeRoutingPolicyRule(rule RoutingPolicyRule) RoutingPolicyRule {
	out := RoutingPolicyRule{IncludeRemainingProviders: rule.IncludeRemainingProviders}
	seenProviders := make(map[string]struct{}, len(rule.Route))
	for _, route := range rule.Route {
		provider := strings.ToLower(strings.TrimSpace(route.Provider))
		if provider == "" {
			continue
		}
		if _, exists := seenProviders[provider]; exists {
			continue
		}
		seenProviders[provider] = struct{}{}
		seenAuthIDs := make(map[string]struct{}, len(route.AuthOrder))
		var authOrder []string
		for _, authID := range route.AuthOrder {
			authID = strings.TrimSpace(authID)
			if authID == "" {
				continue
			}
			if _, exists := seenAuthIDs[authID]; exists {
				continue
			}
			seenAuthIDs[authID] = struct{}{}
			authOrder = append(authOrder, authID)
		}
		out.Route = append(out.Route, RoutingPolicyRoute{Provider: provider, AuthOrder: authOrder, IncludeRemainingAuth: route.IncludeRemainingAuth})
	}
	return out
}
