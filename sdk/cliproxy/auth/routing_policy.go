package auth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// routingFallbackUnavailable marks a route step that never reached upstream because its
// credential could not be selected (unknown, disabled or missing executor). It always falls back.
const routingFallbackUnavailable = "unavailable"

const defaultRoutingTraceLimit = 200

type routingAuthFilterContextKey struct{}

// routingAuthFilter restricts credential selection to what the routing policy allows.
type routingAuthFilter struct {
	// allowOnly maps a provider to the only auth IDs it may use.
	allowOnly map[string]map[string]struct{}
	// exclude lists auth IDs already covered by earlier route steps.
	exclude map[string]struct{}
}

func (f *routingAuthFilter) allows(auth *Auth) bool {
	if f == nil || auth == nil {
		return true
	}
	if _, excluded := f.exclude[auth.ID]; excluded {
		return false
	}
	if allowed, restricted := f.allowOnly[strings.ToLower(strings.TrimSpace(auth.Provider))]; restricted {
		_, ok := allowed[auth.ID]
		return ok
	}
	return true
}

func withRoutingAuthFilter(ctx context.Context, filter *routingAuthFilter) context.Context {
	if filter == nil {
		return ctx
	}
	return context.WithValue(ctx, routingAuthFilterContextKey{}, filter)
}

// routingStep is one entry of the ordered route plan. A step with an authID pins that
// credential; otherwise the normal scheduler picks among providers, skipping exclude.
type routingStep struct {
	providers []string
	authID    string
	exclude   []string
}

// routingPlan is the operation-independent route for one request. Every step runs through
// the normal execution pipeline, so aliases, interceptors, retries and payload rules apply.
type routingPlan struct {
	manager   *Manager
	steps     []routingStep
	providers []string
	// creditsProviders is what the credits fallback may use. It can be non-empty when providers is
	// empty: a unified plan whose free routes are all exhausted at build time still reaches credits.
	creditsProviders []string
	filter           *routingAuthFilter
	eligible         func(string) bool
	fallbackOn       map[string]struct{}
	traceLimit       int
	trace            RoutingTrace
}

// routingPlanFor returns nil when no plan applies: no providers, Home mode, a caller-pinned
// credential, or neither a unified model nor an enabled routing policy.
// Traces are kept only while the routing policy is enabled.
func (m *Manager) routingPlanFor(operation string, providers []string, model string, opts cliproxyexecutor.Options) *routingPlan {
	if m == nil || len(providers) == 0 || m.HomeEnabled() || pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return nil
	}
	var plan *routingPlan
	switch {
	case cfg.Routing.UnifiedModels.Find(thinking.ParseSuffix(model).ModelName) != nil:
		plan = m.unifiedRoutingPlan(cfg, providers, model)
	case cfg.Routing.Policy.Enabled:
		plan = buildRoutingPlan(cfg.Routing.Policy, providers, model)
	default:
		return nil
	}
	if cfg.Routing.Policy.Enabled {
		plan.manager = m
	}
	plan.trace = RoutingTrace{
		ID:         uuid.NewString(),
		Timestamp:  time.Now().UTC(),
		Operation:  operation,
		Model:      strings.TrimSpace(model),
		Providers:  append([]string(nil), plan.providers...),
		FallbackOn: sortedTriggerList(plan.fallbackOn),
	}
	return plan
}

func buildRoutingPlan(policy internalconfig.RoutingPolicy, providers []string, model string) *routingPlan {
	plan := &routingPlan{
		fallbackOn: routingFallbackTriggers(policy.Fallback),
		traceLimit: routingTraceLimit(policy.Observability),
	}

	rule := policy.Defaults
	trimmed := strings.TrimSpace(model)
	if override, ok := policy.ModelOverrides[trimmed]; ok {
		rule = override
	} else if override, ok := policy.ModelOverrides[strings.TrimSpace(thinking.ParseSuffix(trimmed).ModelName)]; ok {
		rule = override
	}

	input := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		input[strings.ToLower(strings.TrimSpace(provider))] = struct{}{}
	}
	routed := make(map[string]struct{}, len(rule.Route))
	allowOnly := make(map[string]map[string]struct{})
	for _, route := range rule.Route {
		provider := strings.ToLower(strings.TrimSpace(route.Provider))
		if _, ok := input[provider]; !ok {
			continue
		}
		if _, dup := routed[provider]; dup {
			continue
		}
		routed[provider] = struct{}{}
		plan.providers = append(plan.providers, provider)

		explicit := make([]string, 0, len(route.AuthOrder))
		seen := make(map[string]struct{}, len(route.AuthOrder))
		for _, raw := range route.AuthOrder {
			authID := strings.TrimSpace(raw)
			if authID == "" {
				continue
			}
			if _, dup := seen[authID]; dup {
				continue
			}
			seen[authID] = struct{}{}
			explicit = append(explicit, authID)
			plan.steps = append(plan.steps, routingStep{providers: []string{provider}, authID: authID})
		}
		if len(explicit) == 0 || route.IncludeRemainingAuth {
			plan.steps = append(plan.steps, routingStep{providers: []string{provider}, exclude: explicit})
		} else {
			allowOnly[provider] = seen
		}
	}
	// A rule with no routes is absent, so every provider is open. A configured route that the
	// request does not offer must not open the omitted providers unless include-remaining-providers is set.
	if rule.IncludeRemainingProviders || len(rule.Route) == 0 {
		var remaining []string
		for _, provider := range providers {
			if _, ok := routed[strings.ToLower(strings.TrimSpace(provider))]; ok {
				continue
			}
			remaining = append(remaining, provider)
		}
		if len(remaining) > 0 {
			plan.steps = append(plan.steps, routingStep{providers: remaining})
			plan.providers = append(plan.providers, remaining...)
		}
	}
	if len(allowOnly) > 0 {
		plan.filter = &routingAuthFilter{allowOnly: allowOnly}
	}
	plan.creditsProviders = plan.providers
	return plan
}

func routingFallbackTriggers(fallback internalconfig.RoutingFallbackPolicy) map[string]struct{} {
	set := make(map[string]struct{})
	for _, trigger := range fallback.On {
		if normalized, ok := internalconfig.NormalizeRoutingFallbackTrigger(trigger); ok {
			set[normalized] = struct{}{}
		}
	}
	if len(set) == 0 {
		for _, trigger := range internalconfig.DefaultRoutingFallbackTriggers() {
			set[trigger] = struct{}{}
		}
	}
	return set
}

func routingTraceLimit(observability internalconfig.RoutingPolicyObservability) int {
	limit := observability.TraceLimit
	if limit <= 0 {
		return defaultRoutingTraceLimit
	}
	return min(limit, internalconfig.MaxRoutingTraceLimit)
}

// retryProviders returns the providers the upstream retry loop and the credits fallback may use.
// An empty result means the policy offers none of the request's providers.
func (p *routingPlan) retryProviders(fallback []string) []string {
	if p == nil {
		return fallback
	}
	return p.providers
}

// creditsFallbackProviders returns the providers the credits fallback may use.
func (p *routingPlan) creditsFallbackProviders(fallback []string) []string {
	if p == nil {
		return fallback
	}
	return p.creditsProviders
}

// routingAuthFilterFromContext returns the plan-wide auth restriction, or nil when none applies.
func routingAuthFilterFromContext(ctx context.Context) *routingAuthFilter {
	if ctx == nil {
		return nil
	}
	filter, _ := ctx.Value(routingAuthFilterContextKey{}).(*routingAuthFilter)
	return filter
}

// context applies the plan-wide auth restrictions to the request context.
func (p *routingPlan) context(ctx context.Context) context.Context {
	if p == nil {
		return ctx
	}
	return withRoutingAuthFilter(ctx, p.filter)
}

func (p *routingPlan) stepContext(ctx context.Context, step routingStep) context.Context {
	if len(step.exclude) == 0 {
		return ctx
	}
	filter := &routingAuthFilter{exclude: make(map[string]struct{}, len(step.exclude))}
	if p.filter != nil {
		filter.allowOnly = p.filter.allowOnly
	}
	for _, authID := range step.exclude {
		filter.exclude[authID] = struct{}{}
	}
	return withRoutingAuthFilter(ctx, filter)
}

func (step routingStep) options(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	if step.authID == "" {
		return opts
	}
	opts.Metadata = cloneRequestMetadata(opts.Metadata)
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = step.authID
	return opts
}

func (p *routingPlan) allowsFallback(reason string) bool {
	switch reason {
	case "":
		return false
	case routingFallbackUnavailable:
		return true
	}
	if _, ok := p.fallbackOn[reason]; ok {
		return true
	}
	// Many providers report quota exhaustion as 429, so "exhausted" also covers rate limits.
	if reason == internalconfig.RoutingFallbackRateLimited {
		_, ok := p.fallbackOn[internalconfig.RoutingFallbackExhausted]
		return ok
	}
	return false
}

// classifyRoutingFailure maps an execution error to a fallback trigger using only
// structured signals: error types, status codes and wrapped transport errors.
func classifyRoutingFailure(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if ctx != nil && ctx.Err() != nil {
		return ""
	}
	if clienterror.IsCreditExhaustedBody(statusCodeFromError(err), err) {
		return internalconfig.RoutingFallbackExhausted
	}
	if errors.Is(err, context.Canceled) || isRequestInvalidError(err) || isRequestStopError(err) || isRequestTerminatedError(err) {
		return ""
	}
	var cooldown *modelCooldownError
	if errors.As(err, &cooldown) && cooldown != nil {
		return internalconfig.RoutingFallbackExhausted
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && !hasUpstreamExecutionAttempt(err) {
		switch authErr.Code {
		case "auth_not_found", "auth_unavailable", "executor_not_found":
			return routingFallbackUnavailable
		}
	}
	status := statusCodeFromError(err)
	switch {
	case status == http.StatusPaymentRequired:
		return internalconfig.RoutingFallbackExhausted
	case status == http.StatusTooManyRequests:
		return internalconfig.RoutingFallbackRateLimited
	case status >= 500 && status <= 599:
		return internalconfig.RoutingFallbackServerError
	case status != 0:
		return ""
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
		return internalconfig.RoutingFallbackTransportError
	}
	return ""
}

// runRoutingRound runs one upstream retry round. Without a plan it is a single call to once;
// with a plan it walks the route steps, carrying the round's credential budget and attempted
// set, and stops at the first failure the policy does not allow to fall back.
func runRoutingRound[T any](ctx context.Context, plan *routingPlan, providers []string, opts cliproxyexecutor.Options, maxRetryCredentials int, attempted map[string]struct{}, once func(context.Context, []string, cliproxyexecutor.Options, int) (T, error)) (T, error) {
	if plan == nil {
		return once(ctx, providers, opts, maxRetryCredentials)
	}
	var zero T
	var lastErr error
	for _, step := range plan.steps {
		if plan.eligible != nil && !plan.eligible(step.authID) {
			continue
		}
		budget := maxRetryCredentials
		if maxRetryCredentials > 0 {
			budget = maxRetryCredentials - len(attempted)
			if budget <= 0 {
				break
			}
		}
		out, err := once(plan.stepContext(ctx, step), step.providers, step.options(opts), budget)
		if err == nil {
			plan.recordAttempt(step, nil, "", false)
			return out, nil
		}
		reason := classifyRoutingFailure(ctx, err)
		fallback := plan.allowsFallback(reason)
		plan.recordAttempt(step, err, reason, fallback)
		if !fallback {
			return zero, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	return zero, lastErr
}

// allowsCreditsFallback is the entry gate for the credits lane: the failure that ended the plain
// round must match the routing policy's fallback triggers. Per-credential failures do not use it.
func (p *routingPlan) allowsCreditsFallback(ctx context.Context, err error) bool {
	if isRequestTerminatedError(err) || isRequestStopError(err) || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	return p == nil || p.allowsFallback(classifyRoutingFailure(ctx, err))
}
