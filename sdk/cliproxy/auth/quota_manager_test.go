package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// quotaManagerCodex registers a Codex credential with a passive observation and
// the given models, through a real Manager.
func quotaManagerCodex(t *testing.T, m *Manager, id string, observedAt time.Time, usedPercent float64, priority string, models ...string) *Auth {
	t.Helper()
	if m.executors["codex"] == nil {
		m.RegisterExecutor(&quotaTestExecutor{id: "codex"})
	}
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	attrs := map[string]string{AttributeWeight: "1"}
	if priority != "" {
		attrs["priority"] = priority
	}
	auth := &Auth{
		ID: id, Provider: "codex", Status: StatusActive, Attributes: attrs,
		Metadata: map[string]any{"access_token": "tok-" + id, "account_id": "acct-" + id},
	}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register(%s): %v", id, err)
	}
	if observedAt.IsZero() {
		return auth
	}
	setQuotaObservation(t, m, id, QuotaState{ObservedAt: observedAt, Signals: usageSignals(usedPercent, observedAt.Add(time.Hour))})
	return auth
}

func setQuotaObservation(t *testing.T, m *Manager, id string, quota QuotaState) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[id]
	if auth == nil {
		t.Fatalf("auth %s not registered", id)
	}
	auth.Quota = quota
}

// Finding 1: selector probes and recovery probes share one bounded budget.
func TestCreditQuotaRecoveryProbesShareBoundedBudget(t *testing.T) {
	selector := NewQuotaAwareSelector()
	selector.manualRefresh = true
	m := NewManager(nil, selector, nil)
	clock := time.Date(2026, time.October, 28, 20, 0, 0, 0, time.UTC)
	m.creditRecovery.now = func() time.Time { return clock }

	var inflight, peak atomic.Int32
	release := make(chan struct{})
	m.creditRecovery.fetch = func(context.Context, codexUsageCredential) (http.Header, error) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		return creditProbeHeaders(true, false, "10"), nil
	}
	const cooling = 6
	for i := 0; i < cooling; i++ {
		id := fmt.Sprintf("codex-bounded-%d", i)
		auth := &Auth{ID: id, Provider: "codex", Status: StatusActive}
		if _, err := m.Register(WithSkipPersist(context.Background()), auth); err != nil {
			t.Fatal(err)
		}
		setCreditQuotaCooldown(t, m, id, clock.Add(time.Hour))
	}
	ctx := context.Background()
	m.sweepCreditQuotaRecovery(ctx)
	clock = clock.Add(creditQuotaRecoveryTick)
	due := m.sweepCreditQuotaRecovery(ctx)
	if len(due) != quotaMaxConcurrent {
		t.Fatalf("sweep scheduled %d probes, want the shared limit %d", len(due), quotaMaxConcurrent)
	}
	clock = clock.Add(creditQuotaRecoveryTick)
	if more := m.sweepCreditQuotaRecovery(ctx); len(more) != 0 {
		t.Fatalf("sweep scheduled %d more probes while the budget is full", len(more))
	}

	var wg sync.WaitGroup
	for _, probe := range due {
		wg.Add(1)
		go func(p creditQuotaProbe) {
			defer wg.Done()
			m.runCreditQuotaProbe(ctx, p)
		}(probe)
	}
	// Probes are in flight; the selector must see the same full budget.
	if selector.budget != m.probeBudget {
		t.Fatal("selector does not share the manager probe budget")
	}
	close(release)
	wg.Wait()
	if got := peak.Load(); got > quotaMaxConcurrent {
		t.Fatalf("peak concurrent probes = %d, want at most %d", got, quotaMaxConcurrent)
	}
}

// Finding 2: early recovery restores the model's own cooldown instead of dropping it.
func TestCreditQuotaRecoveryRestoresIndependentModelCooldown(t *testing.T) {
	manager, id, clock := creditRecoveryFixture(t)
	const model = "gpt-6.1-terra"
	setCreditQuotaCooldown(t, manager, id, clock.Add(time.Hour))
	ownUntil := clock.Add(2 * time.Hour)
	manager.mu.Lock()
	auth := manager.auths[id]
	own := &ModelState{
		Unavailable:    true,
		Status:         StatusError,
		NextRetryAfter: ownUntil,
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: ownUntil},
	}
	auth.ModelStates = map[string]*ModelState{model: {
		Unavailable:               true,
		Status:                    StatusError,
		NextRetryAfter:            clock.Add(time.Hour),
		Quota:                     QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: clock.Add(time.Hour)},
		credentialQuotaPropagated: true,
		credentialQuotaShadow:     own,
	}}
	manager.mu.Unlock()

	ctx := context.Background()
	setCapacityFetch(manager)
	manager.sweepCreditQuotaRecovery(ctx)
	*clock = clock.Add(creditQuotaRecoveryTick)
	due := manager.sweepCreditQuotaRecovery(ctx)
	if len(due) != 1 {
		t.Fatalf("scheduled %d probes, want 1", len(due))
	}
	manager.runCreditQuotaProbe(ctx, due[0])

	updated, _ := manager.GetByID(id)
	state := updated.ModelStates[model]
	if state == nil || !state.Unavailable || !state.NextRetryAfter.Equal(ownUntil) {
		t.Fatalf("model cooldown after early recovery = %+v, want its own cooldown until %v", state, ownUntil)
	}
	if state.Quota.Reason != "quota" {
		t.Fatalf("model quota reason = %q, want the independent quota", state.Quota.Reason)
	}
	if state.credentialQuotaShadow != nil || state.credentialQuotaPropagated {
		t.Fatal("restored model state still carries credential wrapper fields")
	}
}

// Finding 4: a probe started under quota-aware cannot change cooldowns after the strategy changed.
func TestCreditQuotaRecoveryFencedAfterStrategyChange(t *testing.T) {
	manager, id, clock := creditRecoveryFixture(t)
	setCreditQuotaCooldown(t, manager, id, clock.Add(time.Hour))
	setCapacityFetch(manager)
	ctx := context.Background()
	manager.sweepCreditQuotaRecovery(ctx)
	*clock = clock.Add(creditQuotaRecoveryTick)
	due := manager.sweepCreditQuotaRecovery(ctx)
	if len(due) != 1 {
		t.Fatalf("scheduled %d probes, want 1", len(due))
	}

	manager.SetSelector(&RoundRobinSelector{})
	manager.runCreditQuotaProbe(ctx, due[0])

	updated, _ := manager.GetByID(id)
	if !updated.Quota.Exceeded || updated.Quota.Reason != "credential_quota" {
		t.Fatalf("stale probe released the cooldown after strategy change: quota=%+v", updated.Quota)
	}
}

// Finding 3: the quota selector honors the manager's prevalidated candidates, so an
// alias route is judged on each credential's resolved model, as built-in strategies do.
func TestManagerQuotaAwareUsesPrevalidatedCandidatesForAlias(t *testing.T) {
	const routeModel, targetModel = "quota-manager-route", "quota-manager-target"
	selector := NewQuotaAwareSelector()
	selector.manualRefresh = true
	m := NewManager(nil, selector, nil)
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"codex": {{Name: targetModel, Alias: routeModel, Fork: true}},
	})
	now := time.Now()
	a := quotaManagerCodex(t, m, "alias-a-"+t.Name(), now, 10, "", targetModel)
	quotaManagerCodex(t, m, "alias-b-"+t.Name(), now, 60, "", targetModel)
	// A has a model-only cooldown on the route name. The manager resolves candidates to the
	// target model, so A stays a candidate; a re-check against the route name would drop it.
	m.mu.Lock()
	m.auths[a.ID].ModelStates = map[string]*ModelState{routeModel: {
		Unavailable:    true,
		Status:         StatusError,
		NextRetryAfter: now.Add(time.Hour),
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)},
	}}
	m.mu.Unlock()

	picked, err := m.SelectAuth(context.Background(), "codex", routeModel, cliproxyexecutorOptionsForTest())
	if err != nil {
		t.Fatalf("SelectAuth() error = %v", err)
	}
	if picked.ID != a.ID {
		t.Fatalf("picked %s, want %s (best observed credential the manager resolved)", picked.ID, a.ID)
	}
}

// Finding 5: a session bound to a credential that passive observation marks exhausted rebinds.
func TestSessionAffinityRebindsFromPassivelyExhaustedCredential(t *testing.T) {
	const model = "quota-affinity-model"
	quota := NewQuotaAwareSelector()
	quota.manualRefresh = true
	m := NewManager(nil, NewSessionAffinitySelector(quota), nil)
	now := time.Now()
	a := quotaManagerCodex(t, m, "affinity-a-"+t.Name(), now, 10, "", model)
	b := quotaManagerCodex(t, m, "affinity-b-"+t.Name(), now, 60, "", model)

	opts := cliproxyexecutorOptionsWithSession("session-quota-affinity")
	first, err := m.SelectAuth(context.Background(), "codex", model, opts)
	if err != nil || first.ID != a.ID {
		t.Fatalf("first pick = %v, %v; want %s", first, err, a.ID)
	}
	setQuotaObservation(t, m, a.ID, QuotaState{ObservedAt: now.Add(time.Second), Signals: usageSignals(100, now.Add(time.Hour))})

	second, err := m.SelectAuth(context.Background(), "codex", model, opts)
	if err != nil {
		t.Fatalf("second pick error = %v", err)
	}
	if second.ID != b.ID {
		t.Fatalf("second pick = %s, want rebind to %s after %s was exhausted", second.ID, b.ID, a.ID)
	}
}

// Finding 7: a fully exhausted priority tier falls through to the next healthy tier.
func TestManagerQuotaAwareFallsThroughExhaustedTier(t *testing.T) {
	const model = "quota-tier-model"
	selector := NewQuotaAwareSelector()
	selector.manualRefresh = true
	m := NewManager(nil, selector, nil)
	now := time.Now()
	quotaManagerCodex(t, m, "tier-high-"+t.Name(), now, 100, "10", model)
	low := quotaManagerCodex(t, m, "tier-low-"+t.Name(), now, 10, "0", model)

	picked, err := m.SelectAuth(context.Background(), "codex", model, cliproxyexecutorOptionsForTest())
	if err != nil {
		t.Fatalf("SelectAuth() error = %v, want fall-through to the lower tier", err)
	}
	if picked.ID != low.ID {
		t.Fatalf("picked %s, want lower-priority %s", picked.ID, low.ID)
	}
}

func cliproxyexecutorOptionsForTest() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{}
}

func cliproxyexecutorOptionsWithSession(session string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": []string{session}}}
}

// setCapacityFetch makes every usage probe report a healthy window.
func setCapacityFetch(m *Manager) {
	m.creditRecovery.fetch = func(context.Context, codexUsageCredential) (http.Header, error) {
		return creditProbeHeaders(true, false, "10"), nil
	}
}
