package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	routingFallbackExhausted    = "exhausted"
	routingFallbackRateLimited  = "rate_limited"
	routingFallbackServerError  = "server_error"
	routingFallbackTransportErr = "transport_error"

	defaultRoutingTraceLimit = 200
	maxRoutingTraceLimit     = 2000
)

// RoutingPreview describes the resolved routing plan for a model request.
type RoutingPreview struct {
	Model            string                   `json:"model"`
	Providers        []string                 `json:"providers"`
	OrderedProviders []string                 `json:"ordered_providers"`
	Strategy         string                   `json:"strategy"`
	PolicyEnabled    bool                     `json:"policy_enabled"`
	FallbackOn       []string                 `json:"fallback_on,omitempty"`
	ProviderDetails  []RoutingPreviewProvider `json:"provider_details,omitempty"`
}

// RoutingPreviewProvider describes provider-level ordering in a preview.
type RoutingPreviewProvider struct {
	Provider         string   `json:"provider"`
	AuthOrder        []string `json:"auth_order,omitempty"`
	AvailableAuthIDs []string `json:"available_auth_ids,omitempty"`
}

// RoutingTrace captures one route execution lifecycle.
type RoutingTrace struct {
	ID               string                `json:"id"`
	Timestamp        time.Time             `json:"timestamp"`
	Operation        string                `json:"operation"`
	Model            string                `json:"model"`
	Providers        []string              `json:"providers"`
	OrderedProviders []string              `json:"ordered_providers,omitempty"`
	Strategy         string                `json:"strategy"`
	PolicyEnabled    bool                  `json:"policy_enabled"`
	FallbackOn       []string              `json:"fallback_on,omitempty"`
	Attempts         []RoutingTraceAttempt `json:"attempts,omitempty"`
	FinalStatus      string                `json:"final_status"`
	StopReason       string                `json:"stop_reason,omitempty"`
	Error            string                `json:"error,omitempty"`
}

// RoutingTraceAttempt captures one auth execution attempt.
type RoutingTraceAttempt struct {
	Provider       string `json:"provider"`
	AuthID         string `json:"auth_id,omitempty"`
	Stage          string `json:"stage"`
	Success        bool   `json:"success"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	Error          string `json:"error,omitempty"`
	Fallback       bool   `json:"fallback,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
}

type routingAuthCandidate struct {
	Provider string
	AuthID   string
}

type routingExecutionPlan struct {
	Strategy           string
	PolicyEnabled      bool
	OrderedProviders   []string
	ExplicitCandidates []routingAuthCandidate
	FallbackTriggers   map[string]struct{}
	FallbackOn         []string
}

type routingTraceRuntime struct {
	trace RoutingTrace
}

func normalizeRoutingStrategyValue(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "quota-aware", "quotaaware", "quota", "qa":
		return "quota-aware"
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	default:
		return "round-robin"
	}
}

func normalizeFallbackTriggerValue(trigger string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(trigger)) {
	case "exhausted", "quota", "quota_exhausted":
		return routingFallbackExhausted, true
	case "rate_limited", "ratelimited", "rate-limited", "429":
		return routingFallbackRateLimited, true
	case "server_error", "server", "5xx":
		return routingFallbackServerError, true
	case "transport_error", "transport", "network", "timeout":
		return routingFallbackTransportErr, true
	default:
		return "", false
	}
}

func defaultFallbackTriggerSet() (map[string]struct{}, []string) {
	list := []string{
		routingFallbackExhausted,
		routingFallbackRateLimited,
		routingFallbackServerError,
		routingFallbackTransportErr,
	}
	set := make(map[string]struct{}, len(list))
	for _, item := range list {
		set[item] = struct{}{}
	}
	return set, list
}

func containsFallbackTrigger(set map[string]struct{}, trigger string) bool {
	if len(set) == 0 {
		return false
	}
	_, ok := set[trigger]
	return ok
}

func cloneMetadataMap(meta map[string]any) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]any, len(meta))
	for key, value := range meta {
		out[key] = value
	}
	return out
}

func (m *Manager) setRoutingTraceLimitFromConfig(cfg *internalconfig.Config) {
	limit := defaultRoutingTraceLimit
	if cfg != nil {
		if cfgLimit := cfg.Routing.Policy.Observability.TraceLimit; cfgLimit > 0 {
			limit = cfgLimit
		}
	}
	if limit < 1 {
		limit = defaultRoutingTraceLimit
	}
	if limit > maxRoutingTraceLimit {
		limit = maxRoutingTraceLimit
	}
	m.routingTraceLimit.Store(int32(limit))
}

func normalizeModelOverrideKeys(model string) (string, string) {
	trimmed := strings.TrimSpace(model)
	base := strings.TrimSpace(thinking.ParseSuffix(trimmed).ModelName)
	return trimmed, base
}

func normalizeProviderSlice(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	out := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, provider := range input {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func (m *Manager) snapshotEligibleAuthIDsByProvider(providers []string, model string) map[string][]string {
	result := make(map[string][]string)
	if m == nil || len(providers) == 0 {
		return result
	}

	normalizedProviders := normalizeProviderSlice(providers)
	if len(normalizedProviders) == 0 {
		return result
	}
	providerSet := make(map[string]struct{}, len(normalizedProviders))
	for _, provider := range normalizedProviders {
		providerSet[provider] = struct{}{}
	}

	baseModel := strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	reg := registry.GetGlobalRegistry()
	now := time.Now()
	auths := m.snapshotAuths()
	for _, auth := range auths {
		if auth == nil || auth.Disabled {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if _, ok := providerSet[provider]; !ok {
			continue
		}
		if _, okExecutor := m.Executor(provider); !okExecutor {
			continue
		}
		if baseModel != "" && reg != nil && !reg.ClientSupportsModel(auth.ID, baseModel) {
			continue
		}
		blocked, _, _ := isAuthBlockedForModel(auth, model, now)
		if blocked {
			continue
		}
		authID := strings.TrimSpace(auth.ID)
		if authID == "" {
			continue
		}
		result[provider] = append(result[provider], authID)
	}

	for provider := range result {
		sort.Strings(result[provider])
	}
	return result
}

func (m *Manager) buildRoutingExecutionPlan(providers []string, model string, opts cliproxyexecutor.Options) routingExecutionPlan {
	normalizedProviders := normalizeProviderSlice(providers)
	set, fallbackOn := defaultFallbackTriggerSet()
	plan := routingExecutionPlan{
		Strategy:         "round-robin",
		PolicyEnabled:    false,
		OrderedProviders: normalizedProviders,
		FallbackTriggers: set,
		FallbackOn:       fallbackOn,
	}

	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return plan
	}
	plan.Strategy = normalizeRoutingStrategyValue(cfg.Routing.Strategy)

	// Pinned auth should bypass policy ordering to preserve deterministic manual pin behavior.
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return plan
	}

	policy := cfg.Routing.Policy
	if !policy.Enabled {
		return plan
	}

	plan.PolicyEnabled = true
	fallbackSet := make(map[string]struct{}, len(policy.Fallback.On))
	fallbackList := make([]string, 0, len(policy.Fallback.On))
	for _, trigger := range policy.Fallback.On {
		normalized, ok := normalizeFallbackTriggerValue(trigger)
		if !ok {
			continue
		}
		if _, exists := fallbackSet[normalized]; exists {
			continue
		}
		fallbackSet[normalized] = struct{}{}
		fallbackList = append(fallbackList, normalized)
	}
	if len(fallbackSet) > 0 {
		plan.FallbackTriggers = fallbackSet
		plan.FallbackOn = fallbackList
	}

	rule := policy.Defaults
	modelKey, baseModel := normalizeModelOverrideKeys(model)
	if override, ok := policy.ModelOverrides[modelKey]; ok {
		rule = override
	} else if override, ok := policy.ModelOverrides[baseModel]; ok {
		rule = override
	}

	availableAuthsByProvider := m.snapshotEligibleAuthIDsByProvider(normalizedProviders, model)
	inputProviderSet := make(map[string]struct{}, len(normalizedProviders))
	for _, provider := range normalizedProviders {
		inputProviderSet[provider] = struct{}{}
	}

	orderedProviders := make([]string, 0, len(normalizedProviders))
	seenProviders := make(map[string]struct{}, len(normalizedProviders))
	addProvider := func(provider string) {
		if provider == "" {
			return
		}
		if _, ok := seenProviders[provider]; ok {
			return
		}
		seenProviders[provider] = struct{}{}
		orderedProviders = append(orderedProviders, provider)
	}

	explicitCandidates := make([]routingAuthCandidate, 0)
	for _, route := range rule.Route {
		provider := strings.ToLower(strings.TrimSpace(route.Provider))
		if provider == "" {
			continue
		}
		if _, ok := inputProviderSet[provider]; !ok {
			continue
		}
		addProvider(provider)

		availableAuthIDs := availableAuthsByProvider[provider]
		if len(availableAuthIDs) == 0 {
			continue
		}
		availableSet := make(map[string]struct{}, len(availableAuthIDs))
		for _, authID := range availableAuthIDs {
			availableSet[authID] = struct{}{}
		}
		added := make(map[string]struct{}, len(route.AuthOrder))
		for _, rawAuthID := range route.AuthOrder {
			authID := strings.TrimSpace(rawAuthID)
			if authID == "" {
				continue
			}
			if _, ok := availableSet[authID]; !ok {
				continue
			}
			if _, exists := added[authID]; exists {
				continue
			}
			added[authID] = struct{}{}
			explicitCandidates = append(explicitCandidates, routingAuthCandidate{
				Provider: provider,
				AuthID:   authID,
			})
		}
		if route.IncludeRemainingAuth {
			for _, authID := range availableAuthIDs {
				if _, exists := added[authID]; exists {
					continue
				}
				added[authID] = struct{}{}
				explicitCandidates = append(explicitCandidates, routingAuthCandidate{
					Provider: provider,
					AuthID:   authID,
				})
			}
		}
	}

	if rule.IncludeRemainingProviders || len(orderedProviders) == 0 {
		for _, provider := range normalizedProviders {
			addProvider(provider)
		}
	}
	if len(orderedProviders) == 0 {
		orderedProviders = normalizedProviders
	}
	plan.OrderedProviders = orderedProviders
	plan.ExplicitCandidates = explicitCandidates
	return plan
}

func (m *Manager) newRoutingTraceRuntime(operation, model string, providers []string, plan routingExecutionPlan) *routingTraceRuntime {
	return &routingTraceRuntime{
		trace: RoutingTrace{
			ID:               uuid.NewString(),
			Timestamp:        time.Now().UTC(),
			Operation:        operation,
			Model:            strings.TrimSpace(model),
			Providers:        append([]string(nil), providers...),
			OrderedProviders: append([]string(nil), plan.OrderedProviders...),
			Strategy:         plan.Strategy,
			PolicyEnabled:    plan.PolicyEnabled,
			FallbackOn:       append([]string(nil), plan.FallbackOn...),
			Attempts:         make([]RoutingTraceAttempt, 0, 4),
		},
	}
}

func (m *Manager) appendRoutingTraceAttempt(trace *routingTraceRuntime, attempt RoutingTraceAttempt) {
	if trace == nil {
		return
	}
	trace.trace.Attempts = append(trace.trace.Attempts, attempt)
}

func (m *Manager) finalizeRoutingTrace(trace *routingTraceRuntime, finalStatus, stopReason string, err error) {
	if m == nil || trace == nil {
		return
	}
	trace.trace.FinalStatus = strings.TrimSpace(finalStatus)
	trace.trace.StopReason = strings.TrimSpace(stopReason)
	if err != nil {
		trace.trace.Error = strings.TrimSpace(err.Error())
	}
	m.recordRoutingTrace(trace.trace)
}

func (m *Manager) recordRoutingTrace(trace RoutingTrace) {
	if m == nil {
		return
	}
	limit := int(m.routingTraceLimit.Load())
	if limit <= 0 {
		limit = defaultRoutingTraceLimit
	}

	m.routingTraceMu.Lock()
	defer m.routingTraceMu.Unlock()
	m.routingTraces = append(m.routingTraces, trace)
	if len(m.routingTraces) <= limit {
		return
	}
	m.routingTraces = append([]RoutingTrace(nil), m.routingTraces[len(m.routingTraces)-limit:]...)
}

// ListRoutingTraces returns recent routing traces from newest to oldest.
func (m *Manager) ListRoutingTraces(limit int, model, provider string, failedOnly bool) []RoutingTrace {
	if m == nil {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > maxRoutingTraceLimit {
		limit = maxRoutingTraceLimit
	}

	filterModel := strings.TrimSpace(model)
	filterProvider := strings.ToLower(strings.TrimSpace(provider))

	m.routingTraceMu.Lock()
	defer m.routingTraceMu.Unlock()

	result := make([]RoutingTrace, 0, limit)
	for index := len(m.routingTraces) - 1; index >= 0 && len(result) < limit; index-- {
		trace := m.routingTraces[index]
		if failedOnly && strings.EqualFold(trace.FinalStatus, "success") {
			continue
		}
		if filterModel != "" && !strings.EqualFold(strings.TrimSpace(trace.Model), filterModel) {
			continue
		}
		if filterProvider != "" {
			matched := false
			for _, p := range trace.Providers {
				if strings.EqualFold(strings.TrimSpace(p), filterProvider) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		result = append(result, trace)
	}
	return result
}

// PreviewRouting returns the currently resolved route plan for the request.
func (m *Manager) PreviewRouting(model string, providers []string, opts cliproxyexecutor.Options) RoutingPreview {
	normalizedProviders := normalizeProviderSlice(providers)
	plan := m.buildRoutingExecutionPlan(normalizedProviders, model, opts)
	availableByProvider := m.snapshotEligibleAuthIDsByProvider(plan.OrderedProviders, model)

	authOrderByProvider := make(map[string][]string)
	for _, candidate := range plan.ExplicitCandidates {
		authOrderByProvider[candidate.Provider] = append(authOrderByProvider[candidate.Provider], candidate.AuthID)
	}

	details := make([]RoutingPreviewProvider, 0, len(plan.OrderedProviders))
	for _, provider := range plan.OrderedProviders {
		details = append(details, RoutingPreviewProvider{
			Provider:         provider,
			AuthOrder:        append([]string(nil), authOrderByProvider[provider]...),
			AvailableAuthIDs: append([]string(nil), availableByProvider[provider]...),
		})
	}

	return RoutingPreview{
		Model:            strings.TrimSpace(model),
		Providers:        append([]string(nil), normalizedProviders...),
		OrderedProviders: append([]string(nil), plan.OrderedProviders...),
		Strategy:         plan.Strategy,
		PolicyEnabled:    plan.PolicyEnabled,
		FallbackOn:       append([]string(nil), plan.FallbackOn...),
		ProviderDetails:  details,
	}
}

func classifyFallbackReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if isRequestInvalidError(err) {
		return "", false
	}

	status := statusCodeFromError(err)
	if status == http.StatusTooManyRequests {
		return routingFallbackRateLimited, true
	}
	if status >= 500 && status <= 599 {
		return routingFallbackServerError, true
	}
	if strings.Contains(strings.ToLower(err.Error()), "quota") ||
		strings.Contains(strings.ToLower(err.Error()), "exhaust") ||
		strings.Contains(strings.ToLower(err.Error()), "cooldown") {
		return routingFallbackExhausted, true
	}
	if status == 0 && isTransportExecutionError(err) {
		return routingFallbackTransportErr, true
	}
	return "", false
}

func isTransportExecutionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr != nil {
		if netErr.Timeout() {
			return true
		}
		return true
	}

	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return false
	}
	transportHints := []string{
		"timeout",
		"connection reset",
		"connection refused",
		"broken pipe",
		"tls handshake",
		"unexpected eof",
		"eof",
		"no such host",
	}
	for _, hint := range transportHints {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return false
}

func (m *Manager) shouldFallbackAfterExecutionError(err error, plan routingExecutionPlan) (bool, string) {
	if err == nil {
		return false, ""
	}
	if isRequestInvalidError(err) {
		return false, ""
	}
	if !plan.PolicyEnabled {
		return true, "legacy"
	}
	reason, ok := classifyFallbackReason(err)
	if !ok {
		return false, ""
	}
	if containsFallbackTrigger(plan.FallbackTriggers, reason) {
		return true, reason
	}
	// 429 should also honor "exhausted" if enabled.
	if reason == routingFallbackRateLimited && containsFallbackTrigger(plan.FallbackTriggers, routingFallbackExhausted) {
		return true, routingFallbackExhausted
	}
	return false, ""
}

func (m *Manager) pickPinnedCandidate(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, authID string) (*Auth, ProviderExecutor, error) {
	if m == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	localOpts := opts
	localOpts.Metadata = cloneMetadataMap(opts.Metadata)
	if localOpts.Metadata == nil {
		localOpts.Metadata = make(map[string]any, 1)
	}
	localOpts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = authID
	return m.pickNext(ctx, provider, model, localOpts, tried)
}

func traceAttemptFromError(provider, authID, stage string, err error, fallback bool, fallbackReason string) RoutingTraceAttempt {
	attempt := RoutingTraceAttempt{
		Provider:       provider,
		AuthID:         authID,
		Stage:          stage,
		Success:        false,
		Fallback:       fallback,
		FallbackReason: fallbackReason,
	}
	if err != nil {
		attempt.Error = strings.TrimSpace(err.Error())
		attempt.HTTPStatus = statusCodeFromError(err)
	}
	return attempt
}

func traceAttemptSuccess(provider, authID, stage string) RoutingTraceAttempt {
	return RoutingTraceAttempt{
		Provider: provider,
		AuthID:   authID,
		Stage:    stage,
		Success:  true,
	}
}

type routingTraceStorageFields struct {
	routingTraceMu    sync.Mutex
	routingTraces     []RoutingTrace
	routingTraceLimit atomic.Int32
}

func (m *Manager) routingTraceLimitInfo() string {
	if m == nil {
		return "routing trace manager unavailable"
	}
	return fmt.Sprintf("routing trace limit=%d", m.routingTraceLimit.Load())
}
