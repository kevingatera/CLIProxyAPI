package auth

import (
	"context"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const unifiedQuotaFreshness = 10 * time.Minute

type unifiedQuotaSnapshot struct {
	Report    pluginapi.QuotaFetchResponse
	At        time.Time
	Depletion map[string]float64
}
type unifiedTokenWindow struct {
	Minutes [10]int64
	Epochs  [10]int64
}

func unifiedRouteForAuth(cfg *config.Config, a *Auth, model string) *config.UnifiedModelRoute {
	if cfg == nil || a == nil {
		return nil
	}
	unified := cfg.Routing.UnifiedModels.Find(thinking.ParseSuffix(model).ModelName)
	if unified == nil {
		return nil
	}
	for i := range unified.Routes {
		route := &unified.Routes[i]
		if route.Matches(a.Provider, a.Attributes["compat_name"]) && (route.AuthKind == "" || route.AuthKind == a.AuthKind()) && registry.GetGlobalRegistry().ClientSupportsModel(a.ID, route.Source) {
			return route
		}
	}
	return nil
}

// PresentModels changes discovery only; legacy routes remain registered and usable.
func (m *Manager) PresentModels(models []map[string]any) []map[string]any {
	if m == nil {
		return models
	}
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	if cfg == nil || !cfg.Routing.UnifiedModels.Enabled || cfg.Routing.UnifiedModels.ExposeLegacy {
		return models
	}
	out := make([]map[string]any, 0, len(cfg.Routing.UnifiedModels.Models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if id == "" {
			id, _ = model["name"].(string)
			id = strings.TrimPrefix(id, "models/")
		}
		if cfg.Routing.UnifiedModels.Find(id) != nil {
			out = append(out, model)
		}
	}
	return out
}

// SetUnifiedQuotaFetcher lets the host reuse its native account quota adapters.
func (m *Manager) SetUnifiedQuotaFetcher(fetch func(context.Context, *Auth) (pluginapi.QuotaFetchResponse, error)) {
	m.unifiedMu.Lock()
	defer m.unifiedMu.Unlock()
	m.unifiedQuotaFetcher = fetch
}

// SetUnifiedAllowanceRefresher installs an optional background snapshot reader.
func (m *Manager) SetUnifiedAllowanceRefresher(refresh func(context.Context) bool) {
	m.unifiedMu.Lock()
	defer m.unifiedMu.Unlock()
	m.unifiedAllowanceRefresher = refresh
}

// StartUnifiedRouting refreshes outside inference requests. No network work occurs in selection.
func (m *Manager) StartUnifiedRouting(ctx context.Context) {
	m.unifiedMu.Lock()
	if m.unifiedWake != nil {
		m.unifiedMu.Unlock()
		return
	}
	m.unifiedWake = make(chan struct{}, 1)
	wake := m.unifiedWake
	m.unifiedMu.Unlock()
	coreusage.RegisterNamedPlugin("unified-routing", m)
	go func() {
		m.refreshUnifiedQuotas(ctx)
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.refreshUnifiedQuotas(ctx)
			case <-wake:
				m.refreshUnifiedQuotas(ctx)
			}
		}
	}()
}
func (m *Manager) refreshUnifiedQuotas(ctx context.Context) {
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	if cfg == nil || !cfg.Routing.UnifiedModels.Enabled {
		return
	}
	m.unifiedMu.Lock()
	fetch := m.unifiedQuotaFetcher
	refresh := m.unifiedAllowanceRefresher
	m.unifiedMu.Unlock()
	if refresh != nil && refresh(ctx) {
		return
	}
	if fetch == nil {
		return
	}
	var done []chan struct{}
	for _, a := range m.List() {
		if a.Disabled {
			continue
		}
		participates := false
		for _, model := range cfg.Routing.UnifiedModels.Models {
			for _, route := range model.Routes {
				if route.Matches(a.Provider, a.Attributes["compat_name"]) && (route.AuthKind == "" || route.AuthKind == a.AuthKind()) {
					participates = true
				}
			}
		}
		if !participates {
			continue
		}
		finished := make(chan struct{})
		done = append(done, finished)
		go func(a *Auth) {
			defer close(finished)
			report, err := fetch(ctx, a)
			if err == nil {
				m.ObserveUnifiedQuota(a.ID, report, time.Now())
			}
		}(a)
	}
	for _, ch := range done {
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
	}
}

// ObserveUnifiedQuota records provider reports. Unknown and stale capacity never become unlimited capacity.
func (m *Manager) ObserveUnifiedQuota(authID string, report pluginapi.QuotaFetchResponse, now time.Time) {
	m.unifiedMu.Lock()
	defer m.unifiedMu.Unlock()
	if m.unifiedQuota == nil {
		m.unifiedQuota = make(map[string]unifiedQuotaSnapshot)
	}
	old := m.unifiedQuota[authID]
	next := unifiedQuotaSnapshot{Report: report, At: now, Depletion: make(map[string]float64)}
	hours := now.Sub(old.At).Hours()
	if hours >= 1.0/60 && hours <= 1 {
		for _, group := range report.Groups {
			for _, b := range group.Buckets {
				key := group.DisplayName + "/" + b.Window
				for _, og := range old.Report.Groups {
					if og.DisplayName != group.DisplayName {
						continue
					}
					for _, ob := range og.Buckets {
						if ob.Window == b.Window && ob.ResetTime == b.ResetTime && b.RemainingFraction <= ob.RemainingFraction {
							next.Depletion[key] = math.Max(0, (ob.RemainingFraction-b.RemainingFraction)/hours)
						}
					}
				}
			}
		}
	}
	m.unifiedQuota[authID] = next
}

// HandleUsage observes real generation tokens in a bounded ten-minute window per route.
func (m *Manager) HandleUsage(_ context.Context, record coreusage.Record) {
	if record.Failed || !coreusage.GenerateEnabled(record.Generate) || record.AuthID == "" {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	if cfg == nil || cfg.Routing.UnifiedModels.Find(thinking.ParseSuffix(record.Alias).ModelName) == nil {
		return
	}
	tokens := record.Detail.TotalTokens
	if tokens <= 0 {
		tokens = record.Detail.InputTokens + record.Detail.OutputTokens
	}
	if tokens <= 0 {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	minute := at.Unix() / 60
	slot := minute % 10
	if slot < 0 {
		return
	}
	m.unifiedMu.Lock()
	defer m.unifiedMu.Unlock()
	if m.unifiedBurn == nil {
		m.unifiedBurn = make(map[string]*unifiedTokenWindow)
	}
	key := record.AuthID + "/" + thinking.ParseSuffix(record.Alias).ModelName
	window := m.unifiedBurn[key]
	if window == nil {
		window = &unifiedTokenWindow{}
		m.unifiedBurn[key] = window
	}
	if window.Epochs[slot] != minute {
		window.Minutes[slot] = 0
		window.Epochs[slot] = minute
	}
	window.Minutes[slot] += tokens
}

type UnifiedRouteStatus struct {
	Provider         string     `json:"provider"`
	AuthID           string     `json:"auth_id"`
	Model            string     `json:"upstream_model"`
	Plan             string     `json:"plan"`
	Eligible         bool       `json:"eligible"`
	Reason           string     `json:"reason"`
	QuotaKnown       bool       `json:"quota_known"`
	ObservedAt       *time.Time `json:"observed_at,omitempty"`
	Remaining        *float64   `json:"remaining_fraction,omitempty"`
	CapacityScore    float64    `json:"capacity_score"`
	DepletionPerHour float64    `json:"quota_fraction_per_hour"`
	TokensPerMinute  float64    `json:"model_tokens_per_minute"`
	ResetTime        string     `json:"reset_time,omitempty"`
}

func (m *Manager) UnifiedRoutes(model string, now time.Time) []UnifiedRouteStatus {
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	out := []UnifiedRouteStatus{}
	if cfg == nil || cfg.Routing.UnifiedModels.Find(thinking.ParseSuffix(model).ModelName) == nil {
		return out
	}
	for _, a := range m.List() {
		route := unifiedRouteForAuth(cfg, a, model)
		if route == nil {
			continue
		}
		status := UnifiedRouteStatus{Provider: a.Provider, AuthID: a.ID, Model: route.Model, Plan: route.Plan, Eligible: !a.Disabled, Reason: "quota unknown"}
		if _, ok := m.Executor(a.Provider); !ok {
			status.Eligible = false
			status.Reason = "executor unavailable"
		}
		blocked, _, _ := isAuthBlockedForModel(a, model, now)
		upstreamBlocked, _, _ := isAuthBlockedForModel(a, route.Model, now)
		if a.Disabled || blocked || upstreamBlocked {
			status.Eligible = false
			status.Reason = "disabled or cooling down"
		}
		m.unifiedMu.Lock()
		snapshot := m.unifiedQuota[a.ID]
		if burn := m.unifiedBurn[a.ID+"/"+thinking.ParseSuffix(model).ModelName]; burn != nil {
			minute := now.Unix() / 60
			for i, epoch := range burn.Epochs {
				if epoch <= minute && epoch > minute-10 {
					status.TokensPerMinute += float64(burn.Minutes[i]) / 10
				}
			}
		}
		m.unifiedMu.Unlock()
		if !snapshot.At.IsZero() {
			at := snapshot.At
			status.ObservedAt = &at
		}
		if now.Sub(snapshot.At) >= 0 && now.Sub(snapshot.At) <= unifiedQuotaFreshness {
			for _, group := range snapshot.Report.Groups {
				for _, bucket := range group.Buckets {
					if bucket.Scope == "model" && !slices.Contains(bucket.Models, route.Model) {
						continue
					}
					reset, err := time.Parse(time.RFC3339Nano, bucket.ResetTime)
					if err == nil && !reset.After(now) {
						continue
					}
					remaining := bucket.RemainingFraction
					if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 1 {
						continue
					}
					depletion := snapshot.Depletion[group.DisplayName+"/"+bucket.Window]
					pressure := 0.0
					if err == nil && remaining > 0 {
						pressure = depletion * reset.Sub(now).Hours() / remaining
					}
					score := remaining / (1 + pressure)
					if !status.QuotaKnown || score < status.CapacityScore {
						status.Remaining = &remaining
						status.CapacityScore = score
						status.ResetTime = bucket.ResetTime
						status.DepletionPerHour = depletion
					}
					status.QuotaKnown = true
					if remaining <= 0 {
						status.Eligible = false
						status.Reason = "provider allowance exhausted"
					}
				}
			}
		}
		if now.Sub(snapshot.At) >= 0 && now.Sub(snapshot.At) <= unifiedQuotaFreshness {
			credits, count := 0.0, 0
			for _, metric := range snapshot.Report.Summary {
				if metric.Key == "monthlyCredits" || metric.Key == "purchasedCredits" || metric.Key == "freeCredits" {
					credits += metric.Value
					count++
				}
			}
			if count == 3 && credits <= 0 {
				status.Eligible = false
				status.Reason = "provider credit exhausted"
			}
		}
		if status.Eligible && status.QuotaKnown {
			status.Reason = "fresh provider quota; plan and capacity ranked"
		}
		out = append(out, status)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if a.QuotaKnown != b.QuotaKnown {
			return a.QuotaKnown
		}
		// Preserve included plans unless a binding window has less than five percent headroom.
		comfortableA := a.Remaining == nil || *a.Remaining >= .05
		comfortableB := b.Remaining == nil || *b.Remaining >= .05
		includedA := a.Plan == "included" && comfortableA
		includedB := b.Plan == "included" && comfortableB
		if includedA != includedB {
			return includedA
		}
		// Quantization avoids changing providers for insignificant quota fluctuations.
		scoreA, scoreB := int(a.CapacityScore*20), int(b.CapacityScore*20)
		if scoreA != scoreB {
			return scoreA > scoreB
		}
		return a.AuthID < b.AuthID
	})
	return out
}

func (m *Manager) unifiedExecutionPlan(model string, plan routingExecutionPlan) (routingExecutionPlan, bool) {
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	if cfg == nil || cfg.Routing.UnifiedModels.Find(thinking.ParseSuffix(model).ModelName) == nil {
		return plan, false
	}
	plan.Strategy = "unified-adaptive"
	plan.PolicyEnabled = true
	allowedProviders := append([]string(nil), plan.OrderedProviders...)
	plan.ExplicitCandidates = nil
	plan.OrderedProviders = nil
	seen := make(map[string]bool)
	allowed := make(map[string]bool)
	for _, provider := range allowedProviders {
		allowed[provider] = true
	}
	for _, route := range m.UnifiedRoutes(model, time.Now()) {
		if !route.Eligible || !allowed[route.Provider] {
			continue
		}
		plan.ExplicitCandidates = append(plan.ExplicitCandidates, routingAuthCandidate{Provider: route.Provider, AuthID: route.AuthID})
		if !seen[route.Provider] {
			plan.OrderedProviders = append(plan.OrderedProviders, route.Provider)
			seen[route.Provider] = true
		}
	}
	return plan, true
}

func (m *Manager) executeUnifiedCandidates(ctx context.Context, plan routingExecutionPlan, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, trace *routingTraceRuntime, stage string) (cliproxyexecutor.Response, bool, error) {
	var lastErr error
	for _, candidate := range plan.ExplicitCandidates {
		localOpts := opts
		localOpts.Metadata = cloneMetadataMap(opts.Metadata)
		if localOpts.Metadata == nil {
			localOpts.Metadata = map[string]any{}
		}
		localOpts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = candidate.AuthID
		var response cliproxyexecutor.Response
		var err error
		if stage == "count_tokens" {
			response, err = m.executeCountMixedOnce(ctx, []string{candidate.Provider}, req, localOpts, 1, 0, 0)
		} else {
			response, err = m.executeMixedOnce(ctx, []string{candidate.Provider}, req, localOpts, 1, 0, 0)
		}
		if err == nil {
			m.appendRoutingTraceAttempt(trace, traceAttemptSuccess(candidate.Provider, candidate.AuthID, stage))
			return response, true, nil
		}
		lastErr = err
		fallback, reason := m.shouldFallbackAfterExecutionError(err, plan)
		m.appendRoutingTraceAttempt(trace, traceAttemptFromError(candidate.Provider, candidate.AuthID, stage, err, fallback, reason))
		if !fallback {
			return cliproxyexecutor.Response{}, false, err
		}
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no eligible unified route available"}
	}
	return cliproxyexecutor.Response{}, false, lastErr
}

func unifiedUpstreamModel(route *config.UnifiedModelRoute, requested string) string {
	parsed := thinking.ParseSuffix(requested)
	if parsed.HasSuffix {
		return route.Model + "(" + parsed.RawSuffix + ")"
	}
	return route.Model
}

func unifiedBudgetError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != 400 && status != 402 && status != 403 {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "insufficient credits") || strings.Contains(text, "credit balance exhausted")
}

func (m *Manager) wakeUnifiedRouting() {
	m.unifiedMu.Lock()
	wake := m.unifiedWake
	m.unifiedMu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
