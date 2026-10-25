package auth

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	executor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/codexusage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// setupUnifiedTwins registers identical credentials of one provider behind a single included route.
func setupUnifiedTwins(t *testing.T, ids ...string) (*Manager, *routingPolicyTestExecutor) {
	t.Helper()
	return setupUnifiedTwinsWithPolicy(t, config.RoutingPolicy{}, ids...)
}

// setupUnifiedTwinsWithPolicy is setupUnifiedTwins with a routing policy, which may set explicit auth-orders.
func setupUnifiedTwinsWithPolicy(t *testing.T, policy config.RoutingPolicy, ids ...string) (*Manager, *routingPolicyTestExecutor) {
	t.Helper()
	cfg := &config.Config{Routing: config.RoutingConfig{Policy: policy, UnifiedModels: config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{{ID: "unified-test", Routes: []config.UnifiedModelRoute{
		{Provider: "unified-go", Source: "go/native", Model: "go-wire", Plan: "included"},
	}}}}}}
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	exec := &routingPolicyTestExecutor{provider: "unified-go", errs: map[string]error{}}
	m.RegisterExecutor(exec)
	for _, id := range ids {
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "unified-go", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "unified-go", []*registry.ModelInfo{{ID: "go/native"}, {ID: "cliproxy/unified-test"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return m, exec
}

// Identical credentials must take turns: each pick starts after the credential the previous pick used.
// This is the regression for the old pin, which sent every request to the first credential ID.
func TestUnifiedFullTieRotatesConsecutivePicks(t *testing.T) {
	m, exec := setupUnifiedTwins(t, "unified-go-a", "unified-go-b")
	req := executor.Request{Model: "cliproxy/unified-test"}
	for range 4 {
		if _, err := m.Execute(context.Background(), []string{"unified-go"}, req, executor.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	calls := exec.snapshot()
	if len(calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(calls))
	}
	for i, call := range calls {
		want := []string{"unified-go-a", "unified-go-b"}[i%2]
		if call.authID != want {
			t.Fatalf("pick %d used %q, want %q; picks %+v", i, call.authID, want, calls)
		}
	}
}

// Route inspection is observability: reading the ranking must not advance the rotation.
func TestUnifiedRoutesReadDoesNotAdvanceRotation(t *testing.T) {
	m, _ := setupUnifiedTwins(t, "unified-go-a", "unified-go-b")
	now := m.UnifiedRoutes("cliproxy/unified-test", time.Now())
	for range 5 {
		m.UnifiedRoutes("cliproxy/unified-test", time.Now())
	}
	plan := m.routingPlanFor("execute", []string{"unified-go"}, "cliproxy/unified-test", executor.Options{})
	if plan == nil || len(plan.steps) != 2 || plan.steps[0].authID != now[0].AuthID {
		t.Fatalf("observability reads changed the first pick: %+v", plan)
	}
	if len(m.unifiedRotation) != 0 {
		t.Fatalf("building plans without execution recorded rotation state: %+v", m.unifiedRotation)
	}
}

// Rotation advances only for a credential that was really selected. Planning a tie group, or a
// fallback group that a successful request never reached, leaves its state untouched.
func TestUnifiedRotationAdvancesOnlyOnSelectedCredential(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{UnifiedModels: config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{{ID: "unified-test", Routes: []config.UnifiedModelRoute{
		{Provider: "unified-go", Source: "go/native", Model: "go-wire", Plan: "included", Priority: 10},
		{Provider: "unified-alt", Source: "alt/native", Model: "alt-wire", Plan: "included"},
	}}}}}}
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	goExec := &routingPolicyTestExecutor{provider: "unified-go", errs: map[string]error{}}
	altExec := &routingPolicyTestExecutor{provider: "unified-alt", errs: map[string]error{}}
	m.RegisterExecutor(goExec)
	m.RegisterExecutor(altExec)
	for _, a := range []struct{ id, provider, source, wire string }{
		{"unified-go-a", "unified-go", "go/native", "go-wire"},
		{"unified-go-b", "unified-go", "go/native", "go-wire"},
		{"unified-alt-c", "unified-alt", "alt/native", "alt-wire"},
		{"unified-alt-d", "unified-alt", "alt/native", "alt-wire"},
	} {
		if _, err := m.Register(context.Background(), &Auth{ID: a.id, Provider: a.provider, Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.id, a.provider, []*registry.ModelInfo{{ID: a.source}, {ID: "cliproxy/unified-test"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.id) })
	}
	providers := []string{"unified-go", "unified-alt"}
	req := executor.Request{Model: "cliproxy/unified-test"}

	for range 3 {
		m.routingPlanFor("execute", providers, req.Model, executor.Options{})
	}
	if len(m.unifiedRotation) != 0 {
		t.Fatalf("planning advanced rotation: %+v", m.unifiedRotation)
	}

	// The first request is served by the priority group, so only that group records a pick.
	if _, err := m.Execute(context.Background(), providers, req, executor.Options{}); err != nil {
		t.Fatal(err)
	}
	if len(m.unifiedRotation) != 1 || len(goExec.snapshot()) != 1 || goExec.snapshot()[0].authID != "unified-go-a" {
		t.Fatalf("first pick: rotation %+v, go calls %+v", m.unifiedRotation, goExec.snapshot())
	}

	// Both priority credentials fail, so the request reaches the fallback group, which records its own pick.
	goExec.errs["unified-go-a"] = &Error{HTTPStatus: 503, Message: "down"}
	goExec.errs["unified-go-b"] = &Error{HTTPStatus: 503, Message: "down"}
	if _, err := m.Execute(context.Background(), providers, req, executor.Options{}); err != nil {
		t.Fatal(err)
	}
	picked := map[string]bool{}
	for _, id := range m.unifiedRotation {
		picked[id] = true
	}
	if len(m.unifiedRotation) != 2 || !picked["unified-go-a"] || !picked["unified-alt-c"] {
		t.Fatalf("rotation after fallback = %+v, want go-a and alt-c", m.unifiedRotation)
	}
}

// codexUsageJSON builds a wham/usage payload with a primary and a secondary window.
func codexUsageJSON(primaryUsed float64, primaryResetAfter int64, secondaryUsed float64) []byte {
	return []byte(fmt.Sprintf(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":%v,"reset_after_seconds":%d},"secondary_window":{"used_percent":%v,"reset_after_seconds":3600}}}`, primaryUsed, primaryResetAfter, secondaryUsed))
}

// observeCodex stores a wham/usage payload the way the native refresher does: shared parse, read,
// then the model-scoped report for the wire models the credential serves.
func observeCodex(t *testing.T, m *Manager, id string, payload []byte, at time.Time) {
	t.Helper()
	headers, err := codexusage.Parse(payload)
	if err != nil {
		t.Fatal(err)
	}
	m.ObserveUnifiedQuota(id, CodexUnifiedReport(codexusage.Read(headers.Get, at), []string{"codex-wire"}, at), at)
}

// setupCodexUnified registers Codex credentials behind one included unified route.
func setupCodexUnified(t *testing.T, ids ...string) (*Manager, *routingPolicyTestExecutor) {
	t.Helper()
	cfg := &config.Config{Routing: config.RoutingConfig{UnifiedModels: config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{{ID: "unified-test", Routes: []config.UnifiedModelRoute{
		{Provider: "codex", Source: "codex/native", Model: "codex-wire", Plan: "included"},
	}}}}}}
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	exec := &routingPolicyTestExecutor{provider: "codex", errs: map[string]error{}}
	m.RegisterExecutor(exec)
	for _, id := range ids {
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t", "account_id": id}}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "codex/native"}, {ID: "cliproxy/unified-test"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return m, exec
}

func TestUnifiedCodexNativeReportFeedsKnownSaneScore(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m, _ := setupCodexUnified(t, "codex-a")
	observeCodex(t, m, "codex-a", codexUsageJSON(10, 3600, 20), now)
	routes := m.UnifiedRoutes("cliproxy/unified-test", now)
	if len(routes) != 1 || !routes[0].QuotaKnown || !routes[0].Eligible {
		t.Fatalf("routes = %+v", routes)
	}
	// The binding window is the least remaining one: the secondary window at 20% used.
	if routes[0].Remaining == nil || math.Abs(*routes[0].Remaining-.8) > 1e-9 {
		t.Fatalf("remaining = %v, want binding secondary window at .8", routes[0].Remaining)
	}
	if score := routes[0].CapacityScore; !(score > 0 && score <= 1) {
		t.Fatalf("capacity score %v outside (0, 1]", score)
	}
	for _, body := range []string{`[]`, `{"rate_limit":{"allowed":true}}`, `{"other":1}`} {
		if _, err := codexusage.Parse([]byte(body)); err == nil {
			t.Fatalf("Parse(%s) accepted an unusable payload", body)
		}
	}
}

// The passive response observation no longer ranks unified routes; only the native report does.
func TestUnifiedCodexPassiveObservationDoesNotRank(t *testing.T) {
	m, _ := setupCodexUnified(t, "codex-a")
	headers, err := codexusage.Parse(codexUsageJSON(100, 3600, 0))
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.auths["codex-a"].Quota.ObserveResponseHeadersForProvider("codex", headers, time.Now())
	m.mu.Unlock()
	routes := m.UnifiedRoutes("cliproxy/unified-test", time.Now())
	if len(routes) != 1 || routes[0].QuotaKnown || !routes[0].Eligible {
		t.Fatalf("passive observation ranked the route: %+v", routes)
	}
}

// A report applies only to the wire models it lists, so another unified model is not ranked from it.
func TestUnifiedCodexBucketsApplyOnlyToReportedModels(t *testing.T) {
	m, _ := setupCodexUnified(t, "codex-a")
	m.SetConfig(&config.Config{Routing: config.RoutingConfig{UnifiedModels: config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{
		{ID: "unified-test", Routes: []config.UnifiedModelRoute{{Provider: "codex", Source: "codex/native", Model: "codex-wire", Plan: "included"}}},
		{ID: "unified-other", Routes: []config.UnifiedModelRoute{{Provider: "codex", Source: "codex/other", Model: "codex-other", Plan: "included"}}},
	}}}})
	registry.GetGlobalRegistry().RegisterClient("codex-a", "codex", []*registry.ModelInfo{{ID: "codex/native"}, {ID: "codex/other"}, {ID: "cliproxy/unified-test"}, {ID: "cliproxy/unified-other"}})
	now := time.Now()
	observeCodex(t, m, "codex-a", codexUsageJSON(10, 3600, 0), now)
	if routes := m.UnifiedRoutes("cliproxy/unified-test", now); len(routes) != 1 || !routes[0].QuotaKnown {
		t.Fatalf("reported model not ranked: %+v", routes)
	}
	if routes := m.UnifiedRoutes("cliproxy/unified-other", now); len(routes) != 1 || routes[0].QuotaKnown || !routes[0].Eligible {
		t.Fatalf("unreported model ranked from the codex report: %+v", routes)
	}
}

// A near-empty personal account never beats a mostly empty one, on every pick.
func TestUnifiedCodexFivePercentLosesToNinetyPercentConsistently(t *testing.T) {
	m, exec := setupCodexUnified(t, "codex-low", "codex-high")
	now := time.Now()
	observeCodex(t, m, "codex-low", codexUsageJSON(95, 3600, 0), now)
	observeCodex(t, m, "codex-high", codexUsageJSON(10, 3600, 0), now)
	req := executor.Request{Model: "cliproxy/unified-test"}
	for range 4 {
		plan := m.routingPlanFor("execute", []string{"codex"}, req.Model, executor.Options{})
		if plan == nil || len(plan.steps) == 0 || plan.steps[0].authID != "codex-high" {
			t.Fatalf("plan = %+v, want codex-high first", plan)
		}
	}
	if _, err := m.Execute(context.Background(), []string{"codex"}, req, executor.Options{}); err != nil {
		t.Fatal(err)
	}
	if calls := exec.snapshot(); len(calls) != 1 || calls[0].authID != "codex-high" {
		t.Fatalf("calls = %+v", calls)
	}
}

// Exhausted windows are skipped; after the reset passes on the injected clock the account returns.
func TestUnifiedCodexExhaustedSkipsUntilResetWithoutSleeping(t *testing.T) {
	m, _ := setupCodexUnified(t, "codex-a", "codex-b")
	// Plans read the real clock, so observations are stamped with it; UnifiedRoutes takes the injected clock.
	base := time.Now()
	observeCodex(t, m, "codex-a", codexUsageJSON(100, 60, 0), base)
	observeCodex(t, m, "codex-b", codexUsageJSON(10, 3600, 0), base)

	plan := m.routingPlanFor("execute", []string{"codex"}, "cliproxy/unified-test", executor.Options{})
	if plan == nil || len(plan.steps) != 1 || plan.steps[0].authID != "codex-b" {
		t.Fatalf("exhausted account was not skipped: %+v", plan)
	}
	routes := m.UnifiedRoutes("cliproxy/unified-test", base.Add(30*time.Second))
	for _, r := range routes {
		if r.AuthID == "codex-a" && (r.Eligible || r.Reason != "provider allowance exhausted") {
			t.Fatalf("exhausted account still eligible: %+v", r)
		}
	}
	// Two minutes later the reset has passed on the injected clock. The exhausted window is unknown
	// again, so the account is eligible and unknown instead of staying blocked or reading as unlimited.
	later := base.Add(2 * time.Minute)
	routes = m.UnifiedRoutes("cliproxy/unified-test", later)
	for _, r := range routes {
		if r.AuthID == "codex-a" && (!r.Eligible || r.QuotaKnown) {
			t.Fatalf("reset account not returned as unknown: %+v", r)
		}
	}
}

// A passed reset makes the whole account group unknown, even when its other window is still known.
func TestUnifiedCodexPassedResetMakesGroupUnknown(t *testing.T) {
	m, _ := setupCodexUnified(t, "codex-a")
	base := time.Now()
	observeCodex(t, m, "codex-a", codexUsageJSON(10, 60, 50), base)
	if routes := m.UnifiedRoutes("cliproxy/unified-test", base.Add(30*time.Second)); !routes[0].QuotaKnown || routes[0].Remaining == nil || math.Abs(*routes[0].Remaining-.5) > 1e-9 {
		t.Fatalf("before reset: %+v", routes)
	}
	if routes := m.UnifiedRoutes("cliproxy/unified-test", base.Add(2*time.Minute)); routes[0].QuotaKnown || !routes[0].Eligible {
		t.Fatalf("primary reset passed but account still read as known: %+v", routes)
	}
}

// Identical unknown Codex accounts alternate across the streaming path too.
func TestUnifiedFullTieRotatesStreamingPicks(t *testing.T) {
	m, exec := setupUnifiedTwins(t, "unified-go-a", "unified-go-b")
	req := executor.Request{Model: "cliproxy/unified-test"}
	for range 4 {
		stream, err := m.ExecuteStream(context.Background(), []string{"unified-go"}, req, executor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for range stream.Chunks {
		}
	}
	calls := exec.snapshot()
	if len(calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(calls))
	}
	for i, call := range calls {
		if want := []string{"unified-go-a", "unified-go-b"}[i%2]; call.authID != want {
			t.Fatalf("stream pick %d used %q, want %q", i, call.authID, want)
		}
	}
}

// Token counting is not a generation: it reads the rotation without advancing it.
func TestUnifiedCountTokensDoesNotAdvanceRotation(t *testing.T) {
	m, _ := setupUnifiedTwins(t, "unified-go-a", "unified-go-b")
	for range 3 {
		plan := m.routingPlanFor("count_tokens", []string{"unified-go"}, "cliproxy/unified-test", executor.Options{})
		if plan == nil || plan.steps[0].authID != "unified-go-a" {
			t.Fatalf("count_tokens plan = %+v", plan)
		}
	}
	plan := m.routingPlanFor("execute", []string{"unified-go"}, "cliproxy/unified-test", executor.Options{})
	if plan.steps[0].authID != "unified-go-a" {
		t.Fatalf("count_tokens advanced the rotation: %+v", plan)
	}
}

// Priority and included ordering are explicit; rotation only shuffles routes that tie on them.
func TestUnifiedRotationNeverOverridesPriority(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{UnifiedModels: config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{{ID: "unified-test", Routes: []config.UnifiedModelRoute{
		{Provider: "codex", Source: "codex/native", Model: "codex-wire", Plan: "included", Priority: 10},
		{Provider: "unified-go", Source: "go/native", Model: "go-wire", Plan: "included"},
	}}}}}}
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	m.RegisterExecutor(&routingPolicyTestExecutor{provider: "codex"})
	m.RegisterExecutor(&routingPolicyTestExecutor{provider: "unified-go"})
	for _, a := range []*Auth{
		{ID: "codex-a", Provider: "codex", Status: StatusActive},
		{ID: "codex-b", Provider: "codex", Status: StatusActive},
		{ID: "unified-go-a", Provider: "unified-go", Status: StatusActive},
	} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	}
	registry.GetGlobalRegistry().RegisterClient("codex-a", "codex", []*registry.ModelInfo{{ID: "codex/native"}, {ID: "cliproxy/unified-test"}})
	registry.GetGlobalRegistry().RegisterClient("codex-b", "codex", []*registry.ModelInfo{{ID: "codex/native"}, {ID: "cliproxy/unified-test"}})
	registry.GetGlobalRegistry().RegisterClient("unified-go-a", "unified-go", []*registry.ModelInfo{{ID: "go/native"}, {ID: "cliproxy/unified-test"}})
	for range 4 {
		plan := m.routingPlanFor("execute", []string{"codex", "unified-go"}, "cliproxy/unified-test", executor.Options{})
		if plan == nil || len(plan.steps) != 3 {
			t.Fatalf("plan = %+v", plan)
		}
		if plan.steps[0].providers[0] != "codex" || plan.steps[1].providers[0] != "codex" || plan.steps[2].authID != "unified-go-a" {
			t.Fatalf("priority broken: %+v", plan.steps)
		}
	}
}

// The refresher fetches a Codex account through the shared unified fetcher, and only when no fresh
// report exists.
func TestUnifiedRefreshFetchesCodexWithoutFreshReport(t *testing.T) {
	m, _ := setupCodexUnified(t, "codex-a")
	headers, err := codexusage.Parse(codexUsageJSON(10, 3600, 0))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	fetches := 0
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return fetches
	}
	m.SetUnifiedQuotaFetcher(func(_ context.Context, _ *Auth) (pluginapi.QuotaFetchResponse, error) {
		mu.Lock()
		fetches++
		mu.Unlock()
		now := time.Now()
		return CodexUnifiedReport(codexusage.Read(headers.Get, now), []string{"codex-wire"}, now), nil
	})
	m.refreshUnifiedQuotas(context.Background())
	waitUnifiedCondition(t, "first fetch", func() bool { return count() == 1 && m.unifiedQuotaFresh("codex-a", time.Now()) })

	m.refreshUnifiedQuotas(context.Background())
	if count() != 1 {
		t.Fatalf("fetches = %d while the report is fresh", count())
	}

	m.unifiedMu.Lock()
	aged := m.unifiedQuota["codex-a"]
	aged.At = aged.At.Add(-2 * unifiedQuotaFreshness)
	m.unifiedQuota["codex-a"] = aged
	m.unifiedMu.Unlock()
	m.refreshUnifiedQuotas(context.Background())
	waitUnifiedCondition(t, "second fetch", func() bool { return count() == 2 })
}

// A stalled Codex fetch must not block the refresh tick or start a duplicate fetch for the account.
func TestUnifiedRefreshContinuesPastStalledCodexFetch(t *testing.T) {
	m, _ := setupCodexUnified(t, "codex-a")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var mu sync.Mutex
	fetches := 0
	m.SetUnifiedQuotaFetcher(func(_ context.Context, _ *Auth) (pluginapi.QuotaFetchResponse, error) {
		mu.Lock()
		fetches++
		mu.Unlock()
		<-release
		return pluginapi.QuotaFetchResponse{}, context.Canceled
	})
	done := make(chan struct{})
	go func() {
		m.refreshUnifiedQuotas(context.Background())
		m.refreshUnifiedQuotas(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh tick blocked on a stalled Codex fetch")
	}
	waitUnifiedCondition(t, "stalled fetch started", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fetches == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if fetches != 1 {
		t.Fatalf("fetches = %d while the first is in flight, want 1", fetches)
	}
}
