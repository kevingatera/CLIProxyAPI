package auth

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	executor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	usage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type unifiedTestExecutor struct {
	routingPolicyTestExecutor
	models []string
	fail   bool
}

func (e *unifiedTestExecutor) Execute(ctx context.Context, a *Auth, r executor.Request, o executor.Options) (executor.Response, error) {
	e.models = append(e.models, r.Model)
	if e.fail {
		return executor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "capacity exhausted"}
	}
	return executor.Response{Payload: []byte(`{"model":"native"}`)}, nil
}
func (e *unifiedTestExecutor) ExecuteStream(ctx context.Context, a *Auth, r executor.Request, o executor.Options) (*executor.StreamResult, error) {
	_, err := e.Execute(ctx, a, r, o)
	if err != nil {
		return nil, err
	}
	ch := make(chan executor.StreamChunk, 1)
	ch <- executor.StreamChunk{Payload: []byte(`{"model":"native"}`)}
	close(ch)
	return &executor.StreamResult{Chunks: ch}, nil
}
func setupUnified(t *testing.T) (*Manager, *unifiedTestExecutor, *unifiedTestExecutor, *config.Config) {
	t.Helper()
	cfg := &config.Config{Routing: config.RoutingConfig{UnifiedModels: config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{{ID: "unified-test", Routes: []config.UnifiedModelRoute{
		{Provider: "unified-go", Source: "go/native", Model: "go-wire", Plan: "included"},
		{Provider: "unified-paid", Source: "paid/native", Model: "paid-wire", Plan: "metered"},
	}}}}}}
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	goExec := &unifiedTestExecutor{routingPolicyTestExecutor: routingPolicyTestExecutor{provider: "unified-go"}}
	paidExec := &unifiedTestExecutor{routingPolicyTestExecutor: routingPolicyTestExecutor{provider: "unified-paid"}}
	m.RegisterExecutor(goExec)
	m.RegisterExecutor(paidExec)
	for _, a := range []*Auth{{ID: "unified-go-auth", Provider: "unified-go", Status: StatusActive}, {ID: "unified-paid-auth", Provider: "unified-paid", Status: StatusActive}} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		source := "go/native"
		if a.Provider == "unified-paid" {
			source = "paid/native"
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: source}, {ID: "cliproxy/unified-test"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	}
	return m, goExec, paidExec, cfg
}
func reportUnified(remaining float64, reset time.Time) pluginapi.QuotaFetchResponse {
	return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "plan", Buckets: []pluginapi.QuotaBucket{{Window: "weekly", RemainingFraction: remaining, ResetTime: reset.Format(time.RFC3339)}}}}}
}
func TestUnifiedRankingExhaustionStalenessAndPlan(t *testing.T) {
	m, _, _, _ := setupUnified(t)
	now := time.Now()
	reset := now.Add(time.Hour)
	m.ObserveUnifiedQuota("unified-go-auth", reportUnified(.4, reset), now)
	m.ObserveUnifiedQuota("unified-paid-auth", reportUnified(.9, reset), now)
	if got := m.UnifiedRoutes("cliproxy/unified-test", now); got[0].Provider != "unified-go" {
		t.Fatal(got)
	}
	m.ObserveUnifiedQuota("unified-go-auth", reportUnified(0, reset), now)
	got := m.UnifiedRoutes("cliproxy/unified-test", now)
	if got[0].Provider != "unified-paid" || got[1].Eligible {
		t.Fatal(got)
	}
	got = m.UnifiedRoutes("cliproxy/unified-test", now.Add(11*time.Minute))
	if got[0].QuotaKnown || got[1].QuotaKnown || !got[0].Eligible || !got[1].Eligible {
		t.Fatal(got)
	}
	// A reset that already passed cannot keep an exhausted snapshot authoritative.
	m.ObserveUnifiedQuota("unified-go-auth", reportUnified(0, now.Add(-time.Second)), now)
	got = m.UnifiedRoutes("cliproxy/unified-test", now)
	for _, route := range got {
		if route.Provider == "unified-go" && (!route.Eligible || route.QuotaKnown) {
			t.Fatal(got)
		}
	}
}
func TestUnifiedDepletionAndTokenBurn(t *testing.T) {
	m, _, _, _ := setupUnified(t)
	now := time.Now()
	reset := now.Add(2 * time.Hour)
	m.ObserveUnifiedQuota("unified-paid-auth", reportUnified(1, reset), now.Add(-5*time.Minute))
	m.ObserveUnifiedQuota("unified-paid-auth", reportUnified(.8, reset), now)
	m.HandleUsage(context.Background(), usage.Record{AuthID: "unified-paid-auth", Alias: "cliproxy/unified-test", RequestedAt: now, Detail: usage.Detail{TotalTokens: 1000}})
	for _, route := range m.UnifiedRoutes("cliproxy/unified-test", now) {
		if route.Provider == "unified-paid" {
			if route.DepletionPerHour < 2 || route.CapacityScore >= .8 || route.TokensPerMinute != 100 {
				t.Fatal(route)
			}
		}
	}
}
func TestUnifiedExecutionMapsAndFallsBack(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "response", true: "stream"}[stream], func(t *testing.T) {
			m, goExec, paidExec, _ := setupUnified(t)
			goExec.fail = true
			req := executor.Request{Model: "cliproxy/unified-test(high)", Payload: []byte(`{}`)}
			providers := []string{"unified-go", "unified-paid"}
			var err error
			if stream {
				var result *executor.StreamResult
				result, err = m.ExecuteStream(context.Background(), providers, req, executor.Options{})
				if err == nil {
					for range result.Chunks {
					}
				}
			} else {
				_, err = m.Execute(context.Background(), providers, req, executor.Options{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(goExec.models) != 1 || goExec.models[0] != "go-wire(high)" || len(paidExec.models) != 1 || paidExec.models[0] != "paid-wire(high)" {
				t.Fatalf("go=%v paid=%v", goExec.models, paidExec.models)
			}
		})
	}
}
func TestUnifiedExhaustedRoutesNeverReachExecutor(t *testing.T) {
	m, goExec, paidExec, _ := setupUnified(t)
	now := time.Now()
	for _, id := range []string{"unified-go-auth", "unified-paid-auth"} {
		m.ObserveUnifiedQuota(id, reportUnified(0, now.Add(time.Hour)), now)
	}
	_, err := m.Execute(context.Background(), []string{"unified-go", "unified-paid"}, executor.Request{Model: "cliproxy/unified-test"}, executor.Options{})
	if err == nil || len(goExec.models) > 0 || len(paidExec.models) > 0 {
		t.Fatalf("err=%v go=%v paid=%v", err, goExec.models, paidExec.models)
	}
}
func TestUnifiedPresentationAndLegacy(t *testing.T) {
	m, _, _, cfg := setupUnified(t)
	models := []map[string]any{{"id": "go/native"}, {"id": "cliproxy/unified-test"}}
	if got := m.PresentModels(models); len(got) != 1 || got[0]["id"] != "cliproxy/unified-test" {
		t.Fatal(got)
	}
	cfg.Routing.UnifiedModels.ExposeLegacy = true
	m.SetConfig(cfg)
	if got := m.PresentModels(models); len(got) != 2 {
		t.Fatal(got)
	}
	cfg.Routing.UnifiedModels.BareNames = true
	m.SetConfig(cfg)
	if cfg.Routing.UnifiedModels.Find("unified-test") == nil || cfg.Routing.UnifiedModels.Find("cliproxy/unified-test") != nil {
		t.Fatal("naming toggle")
	}
}

func TestUnifiedCreditExhaustionAndBudgetFailure(t *testing.T) {
	m, _, _, _ := setupUnified(t)
	now := time.Now()
	report := reportUnified(1, now.Add(time.Hour))
	report.Summary = []pluginapi.QuotaMetric{{Key: "monthlyCredits"}, {Key: "purchasedCredits"}, {Key: "freeCredits"}}
	m.ObserveUnifiedQuota("unified-paid-auth", report, now)
	for _, route := range m.UnifiedRoutes("cliproxy/unified-test", now) {
		if route.Provider == "unified-paid" && route.Eligible {
			t.Fatal("zero-credit route eligible")
		}
	}
	// Budget exhaustion is classified by HTTP 402 only; message text is never matched.
	plan := &routingPlan{fallbackOn: routingFallbackTriggers(config.RoutingFallbackPolicy{})}
	ctx := context.Background()
	if !plan.allowsFallback(classifyRoutingFailure(ctx, &Error{HTTPStatus: http.StatusPaymentRequired, Message: "insufficient credits"})) {
		t.Fatal("HTTP 402 budget error did not fall back")
	}
	if plan.allowsFallback(classifyRoutingFailure(ctx, &Error{HTTPStatus: http.StatusBadRequest, Message: "insufficient credits"})) {
		t.Fatal("HTTP 400 with credit text must not fall back")
	}
}

func TestUnifiedUnknownIncludedPlanPrecedesKnownMeteredPlan(t *testing.T) {
	m, included, metered, _ := setupUnified(t)
	now := time.Now()
	m.ObserveUnifiedQuota("unified-paid-auth", reportUnified(1, now.Add(time.Hour)), now)
	routes := m.UnifiedRoutes("cliproxy/unified-test", now)
	if routes[0].Provider != "unified-go" || routes[0].QuotaKnown {
		t.Fatalf("unexpected ranking: %+v", routes)
	}
	for _, streaming := range []bool{false, true} {
		req := executor.Request{Model: "cliproxy/unified-test"}
		var err error
		if streaming {
			_, err = m.ExecuteStream(context.Background(), []string{"unified-paid", "unified-go"}, req, executor.Options{})
		} else {
			_, err = m.Execute(context.Background(), []string{"unified-paid", "unified-go"}, req, executor.Options{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(included.models) != 2 || len(metered.models) != 0 {
		t.Fatalf("included attempts %d, metered attempts %d", len(included.models), len(metered.models))
	}
}

func TestUnifiedPriorityWithinIncludedPlans(t *testing.T) {
	m, _, _, cfg := setupUnified(t)
	cfg.Routing.UnifiedModels.Models[0].Routes[0].Priority = 10
	cfg.Routing.UnifiedModels.Models[0].Routes[1].Plan = "included"
	m.SetConfig(cfg)
	now := time.Now()
	m.ObserveUnifiedQuota("unified-paid-auth", reportUnified(1, now.Add(time.Hour)), now)
	routes := m.UnifiedRoutes("cliproxy/unified-test", now)
	if routes[0].Provider != "unified-go" {
		t.Fatalf("priority ignored: %+v", routes)
	}
	m.ObserveUnifiedQuota("unified-go-auth", reportUnified(0, now.Add(time.Hour)), now)
	routes = m.UnifiedRoutes("cliproxy/unified-test", now)
	if routes[0].Provider != "unified-paid" || routes[1].Eligible {
		t.Fatalf("exhaustion ignored: %+v", routes)
	}
}

// A report whose source declares an earlier expiry must stop being fresh at that expiry,
// even inside the ten-minute local window.
func TestUnifiedQuotaObservationHonorsEarlierExpiry(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	report := reportUnified(0.5, now.Add(time.Hour))
	m.ObserveUnifiedQuotaUntil("short", report, now, now.Add(time.Minute))
	if !m.unifiedQuotaFresh("short", now.Add(30*time.Second)) {
		t.Fatal("report rejected before its declared expiry")
	}
	if m.unifiedQuotaFresh("short", now.Add(2*time.Minute)) {
		t.Fatal("report outlived its declared expiry")
	}
	m.ObserveUnifiedQuota("local", report, now)
	if !m.unifiedQuotaFresh("local", now.Add(5*time.Minute)) {
		t.Fatal("report without an expiry rejected inside the freshness window")
	}
}

// A service snapshot that covers only some credentials must not stop the native refresh
// for the rest.
func TestUnifiedRefreshFetchesCredentialsTheServiceDidNotCover(t *testing.T) {
	m, _, _, _ := setupUnified(t)
	now := time.Now()
	var mu sync.Mutex
	fetched := map[string]int{}
	m.SetUnifiedQuotaFetcher(func(_ context.Context, a *Auth) (pluginapi.QuotaFetchResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		fetched[a.ID]++
		return reportUnified(0.5, now.Add(time.Hour)), nil
	})
	covered := make(chan struct{}, 1)
	m.SetUnifiedAllowanceRefresher(func(context.Context) {
		m.ObserveUnifiedQuotaUntil("unified-go-auth", reportUnified(0.9, now.Add(time.Hour)), now, now.Add(time.Hour))
		select {
		case covered <- struct{}{}:
		default:
		}
	})
	m.refreshUnifiedQuotas(context.Background())
	// Native fetches run detached, so wait for the uncovered credential's report to land.
	waitUnifiedCondition(t, "uncovered paid report", func() bool { return m.unifiedQuotaFresh("unified-paid-auth", time.Now()) })
	mu.Lock()
	uncovered := fetched["unified-paid-auth"]
	mu.Unlock()
	if uncovered != 1 {
		t.Fatalf("paid fetches=%d; the uncovered credential should be fetched once", uncovered)
	}
	// The allowance read runs beside native fetches, so wait for it before judging coverage.
	<-covered
	mu.Lock()
	fetched = map[string]int{}
	mu.Unlock()
	m.refreshUnifiedQuotas(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(fetched) != 0 {
		t.Fatalf("fetches=%v after coverage; both credentials are fresh and should not be fetched", fetched)
	}
}

// A stalled allowance service must not hold back native quota fetches, and a second cycle
// must not start another service call while the first is still blocked.
func TestUnifiedRefreshNotBlockedByStalledAllowanceService(t *testing.T) {
	m, _, _, _ := setupUnified(t)
	now := time.Now()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var refreshCalls atomic.Int32
	m.SetUnifiedAllowanceRefresher(func(context.Context) {
		refreshCalls.Add(1)
		<-release
	})
	fetched := make(chan string, 4)
	m.SetUnifiedQuotaFetcher(func(_ context.Context, a *Auth) (pluginapi.QuotaFetchResponse, error) {
		fetched <- a.ID
		return reportUnified(0.5, now.Add(time.Hour)), nil
	})

	done := make(chan struct{})
	go func() {
		m.refreshUnifiedQuotas(context.Background())
		m.refreshUnifiedQuotas(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("refresh cycle blocked on the allowance service")
	}
	// The service never answers, so both credentials are uncovered and fetched natively.
	got := map[string]bool{<-fetched: true, <-fetched: true}
	if !got["unified-go-auth"] || !got["unified-paid-auth"] {
		t.Fatalf("native fetches = %v, want both credentials", got)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("allowance refresh calls = %d, want 1 while the first call is stalled", got)
	}
}

// An older service snapshot must not readmit a credential that a newer native report marked exhausted.
func TestUnifiedObservationRejectsOlderSnapshot(t *testing.T) {
	m, _, _, _ := setupUnified(t)
	now := time.Now()
	reset := now.Add(time.Hour)
	m.ObserveUnifiedQuota("unified-go-auth", reportUnified(0, reset), now)
	m.ObserveUnifiedQuotaUntil("unified-go-auth", reportUnified(1, reset), now.Add(-time.Minute), reset)
	for _, route := range m.UnifiedRoutes("cliproxy/unified-test", now) {
		if route.Provider == "unified-go" && route.Eligible {
			t.Fatalf("older snapshot readmitted an exhausted credential: %+v", route)
		}
	}
}

// waitUnifiedCondition polls for a background fetch to land, since refresh ticks no longer join them.
func waitUnifiedCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// One stalled native fetch must not hold back later refresh ticks. The stalled credential stays
// single-flight while the other credential is refetched on the second tick.
func TestUnifiedRefreshContinuesPastStalledFetch(t *testing.T) {
	m, _, _, _ := setupUnified(t)
	now := time.Now()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var mu sync.Mutex
	calls := map[string]int{}
	count := func(id string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[id]
	}
	m.SetUnifiedQuotaFetcher(func(_ context.Context, a *Auth) (pluginapi.QuotaFetchResponse, error) {
		mu.Lock()
		calls[a.ID]++
		mu.Unlock()
		if a.ID == "unified-go-auth" {
			<-release
		}
		return reportUnified(0.5, now.Add(time.Hour)), nil
	})
	tick := func() {
		done := make(chan struct{})
		go func() {
			m.refreshUnifiedQuotas(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("refresh tick blocked on a stalled fetch")
		}
	}
	tick()
	waitUnifiedCondition(t, "paid report", func() bool { return m.unifiedQuotaFresh("unified-paid-auth", time.Now()) })

	// Age the paid report past the freshness window so the second tick has work to do.
	m.unifiedMu.Lock()
	snap := m.unifiedQuota["unified-paid-auth"]
	snap.At = snap.At.Add(-11 * time.Minute)
	m.unifiedQuota["unified-paid-auth"] = snap
	m.unifiedMu.Unlock()

	tick()
	waitUnifiedCondition(t, "second paid fetch", func() bool { return count("unified-paid-auth") == 2 })
	if got := count("unified-go-auth"); got != 1 {
		t.Fatalf("stalled credential fetches = %d, want 1 while its first call is in flight", got)
	}
}
