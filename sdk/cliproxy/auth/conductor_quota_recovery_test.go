package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// creditRecoveryFixture registers one Codex credential and returns a clock the test can move.
func creditRecoveryFixture(t *testing.T) (*Manager, string, *time.Time) {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "codex-credit-recovery", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	clock := time.Date(2026, time.October, 28, 20, 0, 0, 0, time.UTC)
	manager.creditRecovery.now = func() time.Time { return clock }
	return manager, auth.ID, &clock
}

// setCreditQuotaCooldown puts the credential into a credential_quota cooldown that ends at recoverAt.
func setCreditQuotaCooldown(t *testing.T, manager *Manager, id string, recoverAt time.Time) {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	auth := manager.auths[id]
	if auth == nil {
		t.Fatalf("auth %s not registered", id)
	}
	auth.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: recoverAt, BackoffLevel: 1}
	auth.Unavailable = true
	auth.NextRetryAfter = recoverAt
	auth.Status = StatusError
	auth.StatusMessage = "quota exhausted"
}

// creditProbeHeaders builds the X-Codex-* headers that a wham/usage probe produces.
func creditProbeHeaders(allowed, limitReached bool, usedPercent string) http.Header {
	headers := make(http.Header)
	headers.Set(codexSignalAllowed, strconv.FormatBool(allowed))
	headers.Set(codexSignalLimitReached, strconv.FormatBool(limitReached))
	headers.Set(codexSignalPrimary+"Used-Percent", usedPercent)
	headers.Set(codexSignalPrimary+"Reset-After-Seconds", "3600")
	return headers
}

func TestCreditQuotaRecoveryReleasesCooldownWhenUsageShowsCapacity(t *testing.T) {
	manager, id, clock := creditRecoveryFixture(t)
	ctx := context.Background()
	setCreditQuotaCooldown(t, manager, id, clock.Add(time.Hour))

	fetches := 0
	manager.creditRecovery.fetch = func(context.Context, codexUsageCredential) (http.Header, error) {
		fetches++
		return creditProbeHeaders(true, false, "10"), nil
	}

	// The first sweep only records when the cooldown was first seen.
	if due := manager.sweepCreditQuotaRecovery(ctx); len(due) != 0 {
		t.Fatalf("first sweep scheduled %d probes, want none", len(due))
	}
	*clock = clock.Add(creditQuotaRecoveryTick)
	due := manager.sweepCreditQuotaRecovery(ctx)
	if len(due) != 1 {
		t.Fatalf("second sweep scheduled %d probes, want 1", len(due))
	}
	manager.runCreditQuotaProbe(ctx, due[0])
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}

	updated, ok := manager.GetByID(id)
	if !ok || updated == nil {
		t.Fatal("auth missing after recovery")
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("cooldown not released: quota=%+v unavailable=%v next=%v", updated.Quota, updated.Unavailable, updated.NextRetryAfter)
	}
	if updated.Status != StatusActive || updated.StatusMessage != "" {
		t.Fatalf("status = %q message = %q, want active and empty", updated.Status, updated.StatusMessage)
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-6.1-terra", *clock); blocked {
		t.Fatal("credential still blocked for scheduling after release")
	}
	if len(manager.creditRecovery.entries) != 0 {
		t.Fatalf("recovery entries = %d after release, want 0", len(manager.creditRecovery.entries))
	}
}

func TestCreditQuotaRecoveryKeepsCooldownWhileWindowIsExhausted(t *testing.T) {
	manager, id, clock := creditRecoveryFixture(t)
	ctx := context.Background()
	deadline := clock.Add(time.Hour)
	setCreditQuotaCooldown(t, manager, id, deadline)

	fetches := 0
	manager.creditRecovery.fetch = func(context.Context, codexUsageCredential) (http.Header, error) {
		fetches++
		return creditProbeHeaders(false, true, "100"), nil
	}

	manager.sweepCreditQuotaRecovery(ctx)
	*clock = clock.Add(creditQuotaRecoveryTick)
	due := manager.sweepCreditQuotaRecovery(ctx)
	if len(due) != 1 {
		t.Fatalf("scheduled %d probes, want 1", len(due))
	}
	manager.runCreditQuotaProbe(ctx, due[0])

	updated, _ := manager.GetByID(id)
	if !updated.Quota.Exceeded || updated.Quota.Reason != "credential_quota" || !updated.Quota.NextRecoverAt.Equal(deadline) {
		t.Fatalf("exhausted window released the cooldown: %+v", updated.Quota)
	}
	if updated.Quota.ObservedAt.IsZero() {
		t.Fatal("exhausted probe was not stored as the observation")
	}

	// The next probe waits a full refresh interval after an exhausted reading.
	*clock = clock.Add(creditQuotaRecoveryTick)
	if due := manager.sweepCreditQuotaRecovery(ctx); len(due) != 0 {
		t.Fatalf("probe repeated before refresh interval: %d", len(due))
	}
	*clock = clock.Add(quotaRefreshInterval)
	if due := manager.sweepCreditQuotaRecovery(ctx); len(due) != 1 {
		t.Fatalf("probe after refresh interval scheduled %d, want 1", len(due))
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d before the second due probe, want 1", fetches)
	}
}

func TestCreditQuotaRecoveryIgnoresProbeThatStartedBeforeCooldownWasSeen(t *testing.T) {
	manager, id, clock := creditRecoveryFixture(t)
	ctx := context.Background()
	deadline := clock.Add(time.Hour)
	setCreditQuotaCooldown(t, manager, id, deadline)

	// A probe that began at the first sighting cannot prove anything about that cooldown.
	stale := creditQuotaProbe{id: id, coolingSince: *clock, recoverAt: deadline}
	if manager.applyCreditQuotaProbe(ctx, stale, *clock, creditProbeHeaders(true, false, "5")) {
		t.Fatal("probe that did not start after the cooldown was seen released it")
	}
	updated, _ := manager.GetByID(id)
	if !updated.Quota.Exceeded {
		t.Fatal("cooldown released by a stale probe")
	}
}

func TestCreditQuotaRecoveryIgnoresProbeForReplacedCooldown(t *testing.T) {
	manager, id, clock := creditRecoveryFixture(t)
	ctx := context.Background()
	setCreditQuotaCooldown(t, manager, id, clock.Add(time.Hour))

	probe := creditQuotaProbe{id: id, coolingSince: *clock, recoverAt: clock.Add(30 * time.Minute)}
	*clock = clock.Add(time.Minute)
	if manager.applyCreditQuotaProbe(ctx, probe, *clock, creditProbeHeaders(true, false, "5")) {
		t.Fatal("probe for an earlier cooldown released the current one")
	}
}

func TestCreditQuotaRecoveryBacksOffAfterFetchFailure(t *testing.T) {
	manager, id, clock := creditRecoveryFixture(t)
	ctx := context.Background()
	setCreditQuotaCooldown(t, manager, id, clock.Add(time.Hour))

	manager.creditRecovery.fetch = func(context.Context, codexUsageCredential) (http.Header, error) {
		return nil, errors.New("usage endpoint unavailable")
	}
	manager.sweepCreditQuotaRecovery(ctx)
	*clock = clock.Add(creditQuotaRecoveryTick)
	due := manager.sweepCreditQuotaRecovery(ctx)
	if len(due) != 1 {
		t.Fatalf("scheduled %d probes, want 1", len(due))
	}
	manager.runCreditQuotaProbe(ctx, due[0])

	updated, _ := manager.GetByID(id)
	if !updated.Quota.Exceeded {
		t.Fatal("fetch failure released the cooldown")
	}
	entry := manager.creditRecovery.entries[id]
	if entry == nil || entry.failures != 1 || !entry.nextAttempt.After(*clock) {
		t.Fatalf("expected a failure backoff, got %+v", entry)
	}
	*clock = clock.Add(creditQuotaRecoveryTick)
	if due := manager.sweepCreditQuotaRecovery(ctx); len(due) != 0 {
		t.Fatalf("probe ran during backoff: %d", len(due))
	}
}

func TestCreditQuotaRecoveryLeavesOtherCooldownsAlone(t *testing.T) {
	manager, id, clock := creditRecoveryFixture(t)
	ctx := context.Background()
	manager.mu.Lock()
	auth := manager.auths[id]
	auth.ModelStates = map[string]*ModelState{
		"gpt-6.1-terra": {
			Unavailable:               true,
			NextRetryAfter:            clock.Add(time.Hour),
			Quota:                     QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: clock.Add(time.Hour)},
			credentialQuotaPropagated: true,
		},
		"gpt-6-luna": {
			Unavailable:    true,
			NextRetryAfter: clock.Add(time.Hour),
			Quota:          QuotaState{Exceeded: true, Reason: "rate_limit", NextRecoverAt: clock.Add(time.Hour)},
		},
	}
	manager.mu.Unlock()

	// Model-level 429 cooldowns are not credential quota, so no probe is scheduled for them.
	if due := manager.sweepCreditQuotaRecovery(ctx); len(due) != 0 {
		t.Fatalf("non-credential cooldown scheduled %d probes", len(due))
	}

	manager.mu.Lock()
	auth.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: clock.Add(time.Hour)}
	auth.Unavailable = true
	auth.NextRetryAfter = clock.Add(time.Hour)
	manager.mu.Unlock()
	probe := creditQuotaProbe{id: id, coolingSince: *clock, recoverAt: clock.Add(time.Hour)}
	*clock = clock.Add(time.Minute)
	if !manager.applyCreditQuotaProbe(ctx, probe, *clock, creditProbeHeaders(true, false, "5")) {
		t.Fatal("expected release for the credential_quota cooldown")
	}

	updated, _ := manager.GetByID(id)
	if updated.ModelStates["gpt-6.1-terra"].Unavailable || updated.ModelStates["gpt-6.1-terra"].Quota.Exceeded {
		t.Fatal("credential-propagated model cooldown was not released")
	}
	if !updated.ModelStates["gpt-6-luna"].Unavailable || updated.ModelStates["gpt-6-luna"].Quota.Reason != "rate_limit" {
		t.Fatal("unrelated model cooldown was released")
	}
	// The credential stays usable for the released model and blocked for the other one.
	if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-6.1-terra", *clock); blocked {
		t.Fatal("released model is still blocked for scheduling")
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-6-luna", *clock); !blocked {
		t.Fatal("model with an unrelated cooldown became schedulable")
	}
}
