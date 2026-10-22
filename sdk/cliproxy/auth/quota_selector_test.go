package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type quotaTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *quotaTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *quotaTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newQuotaTestSelector(clock *quotaTestClock, fetch func(context.Context, codexUsageCredential, time.Time) (*codexUsageSnapshot, error)) *QuotaAwareSelector {
	s := NewQuotaAwareSelector()
	s.now = clock.Now
	s.fetch = fetch
	s.manualRefresh = true
	return s
}

func quotaTestAuths(ids ...string) []*Auth {
	auths := make([]*Auth, 0, len(ids))
	for _, id := range ids {
		auths = append(auths, &Auth{
			ID:       id,
			Provider: "codex",
			Metadata: map[string]any{"access_token": "tok-" + id, "account_id": "acct-" + id},
		})
	}
	return auths
}

// usageSnapshot reports used percent on a window that resets after resetIn.
func usageSnapshot(now time.Time, usedPercent float64, resetIn time.Duration) *codexUsageSnapshot {
	return &codexUsageSnapshot{
		allowed:       true,
		primaryWindow: codexUsageWindow{usedPercent: usedPercent, hasUsedPercent: true, resetAt: now.Add(resetIn)},
	}
}

func seedQuota(s *QuotaAwareSelector, id string, snapshot *codexUsageSnapshot, fetchedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[id] = &quotaEntry{snapshot: snapshot, fetchedAt: fetchedAt, lastSeen: fetchedAt}
}

func pickCounts(t *testing.T, s *QuotaAwareSelector, auths []*Auth, n int) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for i := 0; i < n; i++ {
		picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[picked.ID]++
	}
	return counts
}

// Finding 1: selection is a cache read; probes happen only in refreshDue.
func TestQuotaAwarePickNeverFetches(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	var calls atomic.Int32
	release := make(chan struct{})
	s := newQuotaTestSelector(clock, func(ctx context.Context, _ codexUsageCredential, now time.Time) (*codexUsageSnapshot, error) {
		calls.Add(1)
		<-release
		return usageSnapshot(now, 10, time.Hour), nil
	})
	auths := quotaTestAuths("a", "b", "c")

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err != nil {
				t.Errorf("Pick() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 0 {
		t.Fatalf("Pick made %d usage calls, want 0", got)
	}

	// Concurrent refresh passes probe each credential once.
	s.refreshDue(context.Background())
	s.refreshDue(context.Background())
	close(release)
	s.wg.Wait()
	if got := calls.Load(); got != 3 {
		t.Fatalf("usage calls = %d, want 3", got)
	}
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 3 {
		t.Fatalf("fresh snapshots were probed again: calls = %d", got)
	}
}

func TestQuotaAwareRefreshIsBounded(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	var calls atomic.Int32
	release := make(chan struct{})
	s := newQuotaTestSelector(clock, func(ctx context.Context, _ codexUsageCredential, now time.Time) (*codexUsageSnapshot, error) {
		calls.Add(1)
		<-release
		return usageSnapshot(now, 10, time.Hour), nil
	})
	pickCounts(t, s, quotaTestAuths("a", "b", "c", "d", "e", "f"), 1)
	s.refreshDue(context.Background())
	s.refreshDue(context.Background())
	if got := calls.Load(); got > quotaMaxConcurrent {
		t.Fatalf("in-flight probes = %d, want at most %d", got, quotaMaxConcurrent)
	}
	close(release)
	s.wg.Wait()
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 6 {
		t.Fatalf("usage calls = %d, want 6 after slots free up", got)
	}
}

// Finding 2: unknown credentials share the shortlist with the best known ones.
func TestQuotaAwareUnknownIsNeitherStarvedNorPreferred(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	seedQuota(s, "best", usageSnapshot(now, 10, time.Hour), now)
	seedQuota(s, "low", usageSnapshot(now, 90, time.Hour), now)
	// "new" has no snapshot.

	counts := pickCounts(t, s, quotaTestAuths("best", "low", "new"), 10)
	if counts["new"] != 5 || counts["best"] != 5 {
		t.Fatalf("picks = %v, want best and new to alternate", counts)
	}
	if counts["low"] != 0 {
		t.Fatalf("picks = %v, want the low scorer outside the shortlist", counts)
	}
}

// Finding 3: a known-exhausted credential is never reintroduced.
func TestQuotaAwareNeverPicksExhausted(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	seedQuota(s, "spent", usageSnapshot(now, 100, time.Hour), now)
	// A successful report with no windows cannot be scored.
	seedQuota(s, "blank", &codexUsageSnapshot{allowed: true}, now)

	counts := pickCounts(t, s, quotaTestAuths("blank", "spent"), 10)
	if counts["spent"] != 0 || counts["blank"] != 10 {
		t.Fatalf("picks = %v, want only blank", counts)
	}
}

func TestQuotaAwareAllExhaustedReturnsCooldown(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	seedQuota(s, "a", usageSnapshot(now, 100, 2*time.Hour), now)
	seedQuota(s, "b", &codexUsageSnapshot{allowed: false, limitReached: true, primaryWindow: codexUsageWindow{resetAt: now.Add(time.Hour)}}, now)

	picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, quotaTestAuths("a", "b"))
	if picked != nil {
		t.Fatalf("picked %s, want no auth", picked.ID)
	}
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatalf("error = %T %v, want modelCooldownError", err, err)
	}
	if cooldown.resetIn != time.Hour {
		t.Fatalf("resetIn = %v, want the earliest reset", cooldown.resetIn)
	}
}

// Finding 4: one eligibility rule for every pool size.
func TestQuotaAwareSingletonHonorsExhaustion(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	seedQuota(s, "only", usageSnapshot(now, 100, time.Hour), now)

	if picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, quotaTestAuths("only")); err == nil {
		t.Fatalf("picked %s, want an error for an exhausted singleton", picked.ID)
	}
}

// Finding 4: failed refreshes back off and stale data stops counting.
func TestQuotaAwareStaleSnapshotExpiresAfterFailedRefreshes(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	var calls atomic.Int32
	s := newQuotaTestSelector(clock, func(context.Context, codexUsageCredential, time.Time) (*codexUsageSnapshot, error) {
		calls.Add(1)
		return nil, errors.New("upstream down")
	})
	now := clock.Now()
	auths := quotaTestAuths("a", "b")
	seedQuota(s, "a", usageSnapshot(now, 100, 24*time.Hour), now)
	seedQuota(s, "b", usageSnapshot(now, 10, 24*time.Hour), now)

	clock.Advance(quotaRefreshInterval)
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("usage calls = %d, want 2", got)
	}
	// Inside the backoff window nothing is retried.
	clock.Advance(quotaRefreshBackoff - time.Second)
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("retried during backoff: calls = %d", got)
	}
	clock.Advance(time.Second)
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 4 {
		t.Fatalf("usage calls = %d, want a retry after backoff", got)
	}

	// Still fresh enough: the exhausted snapshot holds.
	if counts := pickCounts(t, s, auths, 4); counts["a"] != 0 {
		t.Fatalf("picks = %v, want a excluded while its snapshot is fresh", counts)
	}
	// Past the staleness bound both become unknown and share the shortlist.
	clock.Advance(quotaMaxStaleness)
	if counts := pickCounts(t, s, auths, 4); counts["a"] != 2 || counts["b"] != 2 {
		t.Fatalf("picks = %v, want stale snapshots treated as unknown", counts)
	}
}

func TestQuotaAwarePassedResetIsUnknown(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	seedQuota(s, "a", usageSnapshot(now, 100, time.Minute), now)
	clock.Advance(2 * time.Minute)

	picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, quotaTestAuths("a"))
	if err != nil || picked.ID != "a" {
		t.Fatalf("Pick() = %v, %v; want a once its window reset", picked, err)
	}
}

func TestQuotaAwareStopCancelsBackgroundProbe(t *testing.T) {
	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	s := NewQuotaAwareSelector()
	s.fetch = func(ctx context.Context, _ codexUsageCredential, _ time.Time) (*codexUsageSnapshot, error) {
		started <- struct{}{}
		<-ctx.Done()
		done <- ctx.Err()
		return nil, ctx.Err()
	}
	if _, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, quotaTestAuths("a")); err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	<-started
	s.Stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("probe ended with %v, want context.Canceled", err)
	}
	s.wg.Wait()
}

func TestSessionAffinityStopStopsQuotaSelector(t *testing.T) {
	quota := NewQuotaAwareSelector()
	affinity := NewSessionAffinitySelector(quota)
	affinity.Stop()
	quota.mu.Lock()
	stopped := quota.stopped
	quota.mu.Unlock()
	if !stopped {
		t.Fatal("session affinity Stop did not stop the wrapped quota selector")
	}
}

func TestQuotaAwareNonCodexUsesRoundRobin(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	auths := []*Auth{{ID: "x", Provider: "claude"}, {ID: "y", Provider: "claude"}}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		picked, err := s.Pick(context.Background(), "claude", "sonnet", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		seen[picked.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("picks = %v, want round-robin", seen)
	}
	if len(s.entries) != 0 {
		t.Fatalf("tracked %d non-codex auths", len(s.entries))
	}
}

func TestParseCodexUsageUsesGivenClock(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"rate_limit":{"allowed":true,"limit_reached":false,
		"primary_window":{"used_percent":"42.5","reset_after_seconds":600},
		"secondaryWindow":{"usedPercent":7,"resetAt":1800086400}}}`)
	snap, err := parseCodexUsage(body, now)
	if err != nil {
		t.Fatalf("parseCodexUsage() error = %v", err)
	}
	if !snap.primaryWindow.hasUsedPercent || snap.primaryWindow.usedPercent != 42.5 {
		t.Fatalf("primary used = %+v", snap.primaryWindow)
	}
	if !snap.primaryWindow.resetAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("primary reset = %v, want now+10m", snap.primaryWindow.resetAt)
	}
	if !snap.secondaryWindow.resetAt.Equal(time.Unix(1800086400, 0)) || snap.secondaryWindow.usedPercent != 7 {
		t.Fatalf("secondary = %+v", snap.secondaryWindow)
	}
	if _, err := parseCodexUsage([]byte(`[]`), now); err == nil {
		t.Fatal("want an error for a non-object payload")
	}
}
