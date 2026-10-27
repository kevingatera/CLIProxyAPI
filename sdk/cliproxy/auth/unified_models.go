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
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const unifiedQuotaFreshness = 10 * time.Minute

type unifiedQuotaSnapshot struct {
	Report    pluginapi.QuotaFetchResponse
	At        time.Time
	ExpiresAt time.Time
	Depletion map[string]float64
}

// fresh honors the earlier of the reporter's expiry and the local freshness window.
func (s unifiedQuotaSnapshot) fresh(now time.Time) bool {
	if s.At.IsZero() || now.Before(s.At) || now.Sub(s.At) > unifiedQuotaFreshness {
		return false
	}
	return s.ExpiresAt.IsZero() || now.Before(s.ExpiresAt)
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
		if MatchesUnifiedRoute(*route, a) && registry.GetGlobalRegistry().ClientSupportsModel(a.ID, route.Source) {
			return route
		}
	}
	return nil
}

// MatchesUnifiedRoute checks declared credential and subscription requirements.
func MatchesUnifiedRoute(route config.UnifiedModelRoute, a *Auth) bool {
	if a == nil || !route.Matches(a.Provider, a.Attributes["compat_name"]) || (route.AuthKind != "" && route.AuthKind != a.AuthKind()) {
		return false
	}
	if route.SubscriptionOnly {
		active, _ := a.Metadata["is_subs_active"].(bool)
		return a.AuthKind() == AuthKindOAuth && active
	}
	return true
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
// Credentials it leaves without a fresh report are refreshed through the quota fetcher.
func (m *Manager) SetUnifiedAllowanceRefresher(refresh func(context.Context)) {
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
	if refresh != nil {
		m.startUnifiedAllowanceRefresh(ctx, refresh)
	}
	if fetch == nil {
		return
	}
	for _, a := range m.List() {
		if a.Disabled || m.unifiedQuotaFresh(a.ID, time.Now()) {
			continue
		}
		participates := false
		for _, model := range cfg.Routing.UnifiedModels.Models {
			for _, route := range model.Routes {
				if MatchesUnifiedRoute(route, a) {
					participates = true
				}
			}
		}
		if !participates {
			continue
		}
		// Fetches are never joined, so a stalled provider cannot hold back later ticks.
		if m.beginUnifiedFetch(a.ID, time.Now()) {
			go m.runUnifiedFetch(ctx, fetch, a)
		}
	}
}

const (
	unifiedFetchBackoffBase = 30 * time.Second
	unifiedFetchBackoffMax  = 5 * time.Minute
)

// unifiedFetchState is one credential's native quota fetch slot. At most one call is in flight per
// credential, and consecutive failures push the next attempt out with exponential backoff.
type unifiedFetchState struct {
	inFlight bool
	failures int
	retryAt  time.Time
}

// beginUnifiedFetch claims the credential's slot unless a call is in flight or backoff is pending.
func (m *Manager) beginUnifiedFetch(authID string, now time.Time) bool {
	m.unifiedMu.Lock()
	defer m.unifiedMu.Unlock()
	if m.unifiedFetches == nil {
		m.unifiedFetches = make(map[string]*unifiedFetchState)
	}
	st := m.unifiedFetches[authID]
	if st == nil {
		st = &unifiedFetchState{}
		m.unifiedFetches[authID] = st
	}
	if st.inFlight || now.Before(st.retryAt) {
		return false
	}
	st.inFlight = true
	return true
}

func (m *Manager) runUnifiedFetch(ctx context.Context, fetch func(context.Context, *Auth) (pluginapi.QuotaFetchResponse, error), a *Auth) {
	report, err := fetch(ctx, a)
	now := time.Now()
	m.unifiedMu.Lock()
	st := m.unifiedFetches[a.ID]
	st.inFlight = false
	if err != nil {
		st.failures++
		st.retryAt = now.Add(unifiedFetchBackoff(st.failures))
	} else {
		st.failures = 0
		st.retryAt = time.Time{}
	}
	m.unifiedMu.Unlock()
	if err == nil {
		m.ObserveUnifiedQuota(a.ID, report, now)
	}
}

// unifiedFetchBackoff doubles the delay per consecutive failure, capped at the refresh interval.
func unifiedFetchBackoff(failures int) time.Duration {
	delay := unifiedFetchBackoffBase
	for i := 1; i < failures && delay < unifiedFetchBackoffMax; i++ {
		delay *= 2
	}
	return min(delay, unifiedFetchBackoffMax)
}

// startUnifiedAllowanceRefresh runs the allowance read beside native fetches, so a stalled
// service cannot delay them. A call still in flight is not repeated, which bounds the goroutines.
func (m *Manager) startUnifiedAllowanceRefresh(ctx context.Context, refresh func(context.Context)) {
	m.unifiedMu.Lock()
	if m.unifiedAllowanceRunning {
		m.unifiedMu.Unlock()
		return
	}
	m.unifiedAllowanceRunning = true
	m.unifiedMu.Unlock()
	go func() {
		defer func() {
			m.unifiedMu.Lock()
			m.unifiedAllowanceRunning = false
			m.unifiedMu.Unlock()
		}()
		refresh(ctx)
	}()
}

func (m *Manager) unifiedQuotaFresh(authID string, now time.Time) bool {
	m.unifiedMu.Lock()
	defer m.unifiedMu.Unlock()
	return m.unifiedQuota[authID].fresh(now)
}

// ObserveUnifiedQuota records provider reports. Unknown and stale capacity never become unlimited capacity.
func (m *Manager) ObserveUnifiedQuota(authID string, report pluginapi.QuotaFetchResponse, now time.Time) {
	m.ObserveUnifiedQuotaUntil(authID, report, now, time.Time{})
}

// ObserveUnifiedQuotaUntil records a report that its source declared valid only until expiresAt.
func (m *Manager) ObserveUnifiedQuotaUntil(authID string, report pluginapi.QuotaFetchResponse, now, expiresAt time.Time) {
	m.unifiedMu.Lock()
	defer m.unifiedMu.Unlock()
	if m.unifiedQuota == nil {
		m.unifiedQuota = make(map[string]unifiedQuotaSnapshot)
	}
	old := m.unifiedQuota[authID]
	// A late arrival carrying an older observation must not replace a newer report.
	if now.Before(old.At) {
		return
	}
	next := unifiedQuotaSnapshot{Report: report, At: now, ExpiresAt: expiresAt, Depletion: make(map[string]float64)}
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
	Priority         int        `json:"priority,omitempty"`
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
		status := UnifiedRouteStatus{Priority: route.Priority, Provider: a.Provider, AuthID: a.ID, Model: route.Model, Plan: route.Plan, Eligible: !a.Disabled, Reason: "quota unknown"}
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
		if snapshot.fresh(now) {
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
		if snapshot.fresh(now) {
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
		// Preserve included plans unless a binding window has less than five percent headroom.
		comfortableA := a.Remaining == nil || *a.Remaining >= .05
		comfortableB := b.Remaining == nil || *b.Remaining >= .05
		includedA := a.Plan == "included" && comfortableA
		includedB := b.Plan == "included" && comfortableB
		if includedA != includedB {
			return includedA
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.QuotaKnown != b.QuotaKnown {
			return a.QuotaKnown
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

// unifiedRoutingPlan turns the eligible routes of a unified model into pinned steps, included
// plans first. Each step runs through the normal execution path, so a failed step falls back
// by the policy's failure classes (HTTP 402 counts as exhausted).
func (m *Manager) unifiedRoutingPlan(cfg *config.Config, providers []string, model string) *routingPlan {
	plan := &routingPlan{
		fallbackOn: routingFallbackTriggers(cfg.Routing.Policy.Fallback),
		traceLimit: routingTraceLimit(cfg.Routing.Policy.Observability),
		filter:     &routingAuthFilter{allowOnly: make(map[string]map[string]struct{})},
		eligible:   func(authID string) bool { return m.unifiedAuthEligible(model, authID) },
	}
	allowed := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		allowed[provider] = struct{}{}
		plan.filter.allowOnly[provider] = make(map[string]struct{})
	}
	var policy *routingPlan
	if cfg.Routing.Policy.Enabled {
		policy = buildRoutingPlan(cfg.Routing.Policy, providers, model)
	}
	for _, route := range m.UnifiedRoutes(model, time.Now()) {
		provider := strings.ToLower(strings.TrimSpace(route.Provider))
		if _, ok := allowed[provider]; !route.Eligible || !ok {
			continue
		}
		if policy != nil && (!slices.Contains(policy.providers, provider) || !policy.filter.allows(&Auth{ID: route.AuthID, Provider: provider})) {
			continue
		}
		plan.filter.allowOnly[provider][route.AuthID] = struct{}{}
		plan.steps = append(plan.steps, routingStep{providers: []string{provider}, authID: route.AuthID})
		if !slices.Contains(plan.providers, provider) {
			plan.providers = append(plan.providers, provider)
		}
	}
	return plan
}

func unifiedUpstreamModel(route *config.UnifiedModelRoute, requested string) string {
	parsed := thinking.ParseSuffix(requested)
	if parsed.HasSuffix {
		return route.Model + "(" + parsed.RawSuffix + ")"
	}
	return route.Model
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

// unifiedAuthEligible rechecks local quota and cooldown state without holding the auth lock.
// Legacy credits routes intentionally retain their separate cooldown bypass.
func (m *Manager) unifiedAuthEligible(model, authID string) bool {
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	if cfg == nil || cfg.Routing.UnifiedModels.Find(thinking.ParseSuffix(model).ModelName) == nil {
		return true
	}
	for _, route := range m.UnifiedRoutes(model, time.Now()) {
		if route.AuthID == authID {
			return route.Eligible
		}
	}
	return false
}
