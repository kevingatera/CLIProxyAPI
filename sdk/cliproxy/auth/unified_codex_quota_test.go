package auth

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// setupCodexUnified builds a manager whose unified model routes to two Codex credentials
// through the quota-aware selector s. Auths are inserted by pointer so tests can read
// their observations back.
func setupCodexUnified(t *testing.T, s *QuotaAwareSelector, auths ...*Auth) (*Manager, *config.Config) {
	t.Helper()
	cfg := &config.Config{Routing: config.RoutingConfig{UnifiedModels: config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{{ID: "unified-codex", Routes: []config.UnifiedModelRoute{
		{Provider: "codex", Source: "codex/src", Model: "gpt-wire", Plan: "included"},
	}}}}}}
	m := NewManager(nil, s, nil)
	m.SetConfig(cfg)
	m.RegisterExecutor(&routingPolicyTestExecutor{provider: "codex"})
	m.mu.Lock()
	for _, a := range auths {
		a.Status = StatusActive
		m.auths[a.ID] = a
	}
	m.mu.Unlock()
	for _, a := range auths {
		registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: "codex/src"}, {ID: "cliproxy/unified-codex"}})
		id := a.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return m, cfg
}

func codexRouteOrder(m *Manager, now time.Time) []UnifiedRouteStatus {
	return m.UnifiedRoutes("cliproxy/unified-codex", now)
}

func TestUnifiedCodexRanksByObservedHeadroom(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	now := clock.Now()
	s := newQuotaTestSelector(clock, nil)
	a := observed(quotaCodexAuth("codex-a", QuotaState{}), now, usageSignals(60, now.Add(time.Hour)))
	b := observed(quotaCodexAuth("codex-b", QuotaState{}), now, usageSignals(10, now.Add(time.Hour)))
	m, _ := setupCodexUnified(t, s, a, b)

	got := codexRouteOrder(m, now)
	if len(got) != 2 || got[0].AuthID != "codex-b" || got[1].AuthID != "codex-a" {
		t.Fatalf("order = %+v, want codex-b first", got)
	}
	for _, r := range got {
		if !r.QuotaKnown || !r.Eligible || r.Reason != "fresh codex usage; plan and capacity ranked" {
			t.Fatalf("route %s = %+v, want known and eligible", r.AuthID, r)
		}
	}
	if got[0].Remaining == nil || *got[0].Remaining < .89 || *got[0].Remaining > .91 {
		t.Fatalf("remaining = %v, want about .9", got[0].Remaining)
	}
}

func TestUnifiedCodexEqualUsageFallsBackToIDOrder(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	now := clock.Now()
	s := newQuotaTestSelector(clock, nil)
	a := observed(quotaCodexAuth("codex-eq-a", QuotaState{}), now, usageSignals(30, now.Add(time.Hour)))
	b := observed(quotaCodexAuth("codex-eq-b", QuotaState{}), now, usageSignals(30, now.Add(time.Hour)))
	m, _ := setupCodexUnified(t, s, a, b)

	got := codexRouteOrder(m, now)
	if len(got) != 2 || got[0].AuthID != "codex-eq-a" {
		t.Fatalf("order = %+v, want codex-eq-a first", got)
	}
}

func TestUnifiedCodexExhaustedRouteIsIneligible(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	now := clock.Now()
	s := newQuotaTestSelector(clock, nil)
	reset := now.Add(time.Hour)
	a := observed(quotaCodexAuth("codex-ex-a", QuotaState{}), now, usageSignals(100, reset))
	b := observed(quotaCodexAuth("codex-ex-b", QuotaState{}), now, usageSignals(80, reset))
	m, _ := setupCodexUnified(t, s, a, b)

	got := codexRouteOrder(m, now)
	if len(got) != 2 || got[0].AuthID != "codex-ex-b" || !got[0].Eligible {
		t.Fatalf("order = %+v, want codex-ex-b first", got)
	}
	exhausted := got[1]
	if exhausted.AuthID != "codex-ex-a" || exhausted.Eligible || exhausted.Reason != "provider allowance exhausted" || !exhausted.QuotaKnown {
		t.Fatalf("exhausted route = %+v", exhausted)
	}
	if exhausted.ResetTime == "" {
		t.Fatalf("exhausted route has no reset time: %+v", exhausted)
	}
}

func TestUnifiedCodexStaleObservationIsUnknown(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	now := clock.Now()
	s := newQuotaTestSelector(clock, nil)
	stale := observed(quotaCodexAuth("codex-st-a", QuotaState{}), now.Add(-quotaMaxStaleness-time.Minute), usageSignals(5, now.Add(time.Hour)))
	m, _ := setupCodexUnified(t, s, stale)

	got := codexRouteOrder(m, now)
	if len(got) != 1 || got[0].QuotaKnown || !got[0].Eligible || got[0].Reason != "quota unknown" {
		t.Fatalf("stale route = %+v, want unknown and eligible", got)
	}
}

func TestUnifiedCodexFreshSnapshotTakesPrecedence(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	now := clock.Now()
	s := newQuotaTestSelector(clock, nil)
	a := observed(quotaCodexAuth("codex-pr-a", QuotaState{}), now, usageSignals(10, now.Add(time.Hour)))
	m, _ := setupCodexUnified(t, s, a)
	m.ObserveUnifiedQuota("codex-pr-a", reportUnified(.2, now.Add(time.Hour)), now)

	got := codexRouteOrder(m, now)
	if len(got) != 1 || got[0].Remaining == nil || *got[0].Remaining != .2 || got[0].Reason != "fresh provider quota; plan and capacity ranked" {
		t.Fatalf("route = %+v, want the unified snapshot to win", got)
	}
}

func TestUnifiedCodexPlanTracksParticipatingAuths(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	cold := quotaCodexAuth("codex-pl-cold", QuotaState{})
	m, cfg := setupCodexUnified(t, s, cold)

	m.unifiedRoutingPlan(cfg, []string{"codex"}, "cliproxy/unified-codex")
	s.mu.Lock()
	entry := s.entries["codex-pl-cold"]
	s.mu.Unlock()
	if entry == nil || !entry.needsProbe {
		t.Fatalf("entry = %+v, want the participating Codex auth marked for probe", entry)
	}
}

func TestQuotaWatchMarksOnlyUnobservedCodexAuths(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	now := clock.Now()
	s := newQuotaTestSelector(clock, nil)
	fresh := observed(quotaTestAuths("watch-fresh")[0], now, usageSignals(10, now.Add(time.Hour)))
	cold := quotaTestAuths("watch-cold")[0]
	nonCodex := &Auth{ID: "other", Provider: "gemini"}

	s.watch([]*Auth{fresh, cold, nonCodex})

	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries["watch-cold"]; e == nil || !e.needsProbe {
		t.Fatalf("cold entry = %+v, want needsProbe", e)
	}
	if e := s.entries["watch-fresh"]; e == nil || e.needsProbe {
		t.Fatalf("fresh entry = %+v, want no probe", e)
	}
	if _, ok := s.entries["other"]; ok {
		t.Fatal("non-Codex auth was tracked")
	}
}
