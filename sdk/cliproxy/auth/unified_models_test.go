package auth

import (
	"context"
	"net/http"
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
	goExec := &unifiedTestExecutor{routingPolicyTestExecutor: routingPolicyTestExecutor{id: "unified-go"}}
	paidExec := &unifiedTestExecutor{routingPolicyTestExecutor: routingPolicyTestExecutor{id: "unified-paid"}}
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
	plan := routingExecutionPlan{Strategy: "unified-adaptive", PolicyEnabled: true}
	if ok, _ := m.shouldFallbackAfterExecutionError(&Error{HTTPStatus: 400, Message: "insufficient credits"}, plan); !ok {
		t.Fatal("known provider budget error did not fall back")
	}
	if ok, _ := m.shouldFallbackAfterExecutionError(&Error{HTTPStatus: 400, Message: "invalid input"}, plan); ok {
		t.Fatal("invalid input must not fall back")
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
