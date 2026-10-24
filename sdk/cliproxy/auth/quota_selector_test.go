package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
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

type quotaFetchFunc func(context.Context, codexUsageCredential) (http.Header, error)

// newQuotaTestSelector returns a selector driven by clock. Probe results are
// stored through a real Manager (see newQuotaTestManager).
func newQuotaTestSelector(clock *quotaTestClock, fetch quotaFetchFunc) *QuotaAwareSelector {
	s := NewQuotaAwareSelector()
	s.now = clock.Now
	s.fetch = fetch
	s.manualRefresh = true
	return s
}

// newQuotaTestManager wires s to a manager that holds auths by pointer, so a probe
// write-back is visible to the auths the test passes to Pick.
func newQuotaTestManager(s *QuotaAwareSelector, auths []*Auth) *Manager {
	m := NewManager(nil, s, nil)
	m.mu.Lock()
	for _, a := range auths {
		m.auths[a.ID] = a
	}
	m.mu.Unlock()
	return m
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

// usageSignals builds a passive observation as Codex sends it for the primary
// window: used percent plus an absolute reset time.
func usageSignals(usedPercent float64, resetAt time.Time) map[string]string {
	return map[string]string{
		codexSignalAllowed:                  "true",
		codexSignalPrimary + "Used-Percent": strconv.FormatFloat(usedPercent, 'f', -1, 64),
		codexSignalPrimary + "Reset-At":     strconv.FormatInt(resetAt.Unix(), 10),
	}
}

func observed(a *Auth, observedAt time.Time, signals map[string]string) *Auth {
	a.Quota = QuotaState{ObservedAt: observedAt, Signals: signals}
	return a
}

func headerOf(signals map[string]string) http.Header {
	h := make(http.Header, len(signals))
	for k, v := range signals {
		h.Set(k, v)
	}
	return h
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

// Selection is a read of passive observations; probes happen only in refreshDue.
func TestQuotaAwarePickNeverFetches(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	var calls atomic.Int32
	release := make(chan struct{})
	s := newQuotaTestSelector(clock, func(ctx context.Context, _ codexUsageCredential) (http.Header, error) {
		calls.Add(1)
		<-release
		return headerOf(usageSignals(10, clock.Now().Add(time.Hour))), nil
	})
	auths := quotaTestAuths("a", "b", "c")
	newQuotaTestManager(s, auths)

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
	for _, a := range auths {
		if a.Quota.ObservedAt.IsZero() {
			t.Fatalf("auth %s has no observation after its probe", a.ID)
		}
	}
	// Probe results are fresh observations, so no further probes are due.
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 3 {
		t.Fatalf("fresh observations were probed again: calls = %d", got)
	}
}

func TestQuotaAwareRefreshIsBounded(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	var calls atomic.Int32
	release := make(chan struct{})
	s := newQuotaTestSelector(clock, func(ctx context.Context, _ codexUsageCredential) (http.Header, error) {
		calls.Add(1)
		<-release
		return headerOf(usageSignals(10, clock.Now().Add(time.Hour))), nil
	})
	auths := quotaTestAuths("a", "b", "c", "d", "e", "f")
	newQuotaTestManager(s, auths)
	pickCounts(t, s, auths, 1)
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

// Unknown credentials share the shortlist with the best known ones.
func TestQuotaAwareUnknownIsNeitherStarvedNorPreferred(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	auths := []*Auth{
		observed(quotaTestAuths("best")[0], now, usageSignals(10, now.Add(time.Hour))),
		observed(quotaTestAuths("low")[0], now, usageSignals(90, now.Add(time.Hour))),
		quotaTestAuths("new")[0], // no observation
	}

	counts := pickCounts(t, s, auths, 10)
	if counts["new"] != 5 || counts["best"] != 5 {
		t.Fatalf("picks = %v, want best and new to alternate", counts)
	}
	if counts["low"] != 0 {
		t.Fatalf("picks = %v, want the low scorer outside the shortlist", counts)
	}
}

// An exhausted credential is never reintroduced.
func TestQuotaAwareNeverPicksExhausted(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	auths := []*Auth{
		// A successful observation with no usable windows cannot be scored.
		observed(quotaTestAuths("blank")[0], now, map[string]string{codexSignalAllowed: "true"}),
		observed(quotaTestAuths("spent")[0], now, usageSignals(100, now.Add(time.Hour))),
	}

	counts := pickCounts(t, s, auths, 10)
	if counts["spent"] != 0 || counts["blank"] != 10 {
		t.Fatalf("picks = %v, want only blank", counts)
	}
}

func TestQuotaAwareAllExhaustedReturnsCooldown(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	auths := []*Auth{
		observed(quotaTestAuths("a")[0], now, usageSignals(100, now.Add(2*time.Hour))),
		observed(quotaTestAuths("b")[0], now, map[string]string{
			codexSignalAllowed:              "false",
			codexSignalLimitReached:         "true",
			codexSignalPrimary + "Reset-At": strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		}),
	}

	picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
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

func TestQuotaAwareSingletonHonorsExhaustion(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	auths := []*Auth{observed(quotaTestAuths("only")[0], now, usageSignals(100, now.Add(time.Hour)))}

	if picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err == nil {
		t.Fatalf("picked %s, want an error for an exhausted singleton", picked.ID)
	}
}

// Failed probes back off, and an observation older than quotaMaxStaleness stops counting.
func TestQuotaAwareStaleObservationExpiresAfterFailedRefreshes(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	var calls atomic.Int32
	s := newQuotaTestSelector(clock, func(context.Context, codexUsageCredential) (http.Header, error) {
		calls.Add(1)
		return nil, errors.New("upstream down")
	})
	now := clock.Now()
	auths := []*Auth{
		observed(quotaTestAuths("a")[0], now, usageSignals(100, now.Add(24*time.Hour))),
		observed(quotaTestAuths("b")[0], now, usageSignals(10, now.Add(24*time.Hour))),
	}
	newQuotaTestManager(s, auths)

	clock.Advance(quotaRefreshInterval)
	pickCounts(t, s, auths, 1) // marks both credentials for a probe
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("usage calls = %d, want 2", got)
	}
	// Inside the backoff window nothing is retried.
	clock.Advance(quotaRefreshBackoff - time.Second)
	pickCounts(t, s, auths, 1)
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("retried during backoff: calls = %d", got)
	}
	clock.Advance(time.Second)
	pickCounts(t, s, auths, 1)
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 4 {
		t.Fatalf("usage calls = %d, want a retry after backoff", got)
	}

	// Still fresh enough: the exhausted observation holds.
	if counts := pickCounts(t, s, auths, 4); counts["a"] != 0 {
		t.Fatalf("picks = %v, want a excluded while its observation is fresh", counts)
	}
	// Past the staleness bound both become unknown and share the shortlist.
	clock.Advance(quotaMaxStaleness)
	if counts := pickCounts(t, s, auths, 4); counts["a"] != 2 || counts["b"] != 2 {
		t.Fatalf("picks = %v, want stale observations treated as unknown", counts)
	}
}

func TestQuotaAwarePassedResetIsUnknown(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	auths := []*Auth{observed(quotaTestAuths("a")[0], now, usageSignals(100, now.Add(time.Minute)))}
	clock.Advance(2 * time.Minute)

	picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil || picked.ID != "a" {
		t.Fatalf("Pick() = %v, %v; want a once its window reset", picked, err)
	}
}

// Fresh passive observations are used without any probe.
func TestQuotaAwareReadsPassiveObservationWithoutProbe(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	var calls atomic.Int32
	s := newQuotaTestSelector(clock, func(context.Context, codexUsageCredential) (http.Header, error) {
		calls.Add(1)
		return nil, errors.New("should not be called")
	})
	now := clock.Now()
	auths := []*Auth{
		observed(quotaTestAuths("light")[0], now, usageSignals(10, now.Add(time.Hour))),
		observed(quotaTestAuths("heavy")[0], now, usageSignals(60, now.Add(time.Hour))),
	}
	newQuotaTestManager(s, auths)

	counts := pickCounts(t, s, auths, 5)
	if counts["light"] != 5 {
		t.Fatalf("picks = %v, want the lower-usage credential every time", counts)
	}
	s.refreshDue(context.Background())
	s.wg.Wait()
	if got := calls.Load(); got != 0 {
		t.Fatalf("fresh observations triggered %d probes", got)
	}
}

func TestQuotaAwareStopCancelsBackgroundProbe(t *testing.T) {
	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	s := NewQuotaAwareSelector()
	s.fetch = func(ctx context.Context, _ codexUsageCredential) (http.Header, error) {
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

// The Manager wires the probe observer into the selector, including behind session affinity.
func TestManagerBindsQuotaObserver(t *testing.T) {
	quota := NewQuotaAwareSelector()
	m := NewManager(nil, NewSessionAffinitySelector(quota), nil)
	if quota.observe == nil {
		t.Fatal("NewManager did not bind the quota observer through session affinity")
	}
	next := NewQuotaAwareSelector()
	m.SetSelector(next)
	if next.observe == nil {
		t.Fatal("SetSelector did not bind the quota observer")
	}
	quota.Stop()
	next.Stop()
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

// A probe must not replace a newer observation from a live response.
func TestObserveQuotaProbeKeepsNewerObservation(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	auths := []*Auth{observed(quotaTestAuths("a")[0], now.Add(time.Second), usageSignals(10, now.Add(time.Hour)))}
	m := newQuotaTestManager(s, auths)

	m.observeQuotaProbe("a", headerOf(usageSignals(90, now.Add(time.Hour))), now)
	if got := auths[0].Quota.Signals[codexSignalPrimary+"Used-Percent"]; got != "10" {
		t.Fatalf("probe overwrote a newer observation: used = %s", got)
	}
	m.observeQuotaProbe("a", headerOf(usageSignals(90, now.Add(time.Hour))), now.Add(2*time.Second))
	if got := auths[0].Quota.Signals[codexSignalPrimary+"Used-Percent"]; got != "90" {
		t.Fatalf("newer probe not stored: used = %s", got)
	}
}

func TestParseCodexUsageConvertsPayloadToObservation(t *testing.T) {
	body := []byte(`{"rate_limit":{"allowed":true,"limit_reached":false,
		"primary_window":{"used_percent":"42.5","reset_after_seconds":600},
		"secondaryWindow":{"usedPercent":7,"resetAt":1800086400}}}`)
	headers, err := parseCodexUsage(body)
	if err != nil {
		t.Fatalf("parseCodexUsage() error = %v", err)
	}
	want := map[string]string{
		codexSignalAllowed:                         "true",
		codexSignalLimitReached:                    "false",
		codexSignalPrimary + "Used-Percent":        "42.5",
		codexSignalPrimary + "Reset-After-Seconds": "600",
		codexSignalSecondary + "Used-Percent":      "7",
		codexSignalSecondary + "Reset-At":          "1800086400",
	}
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Fatalf("%s = %q, want %q", name, got, value)
		}
	}
	if _, err := parseCodexUsage([]byte(`[]`)); err == nil {
		t.Fatal("want an error for a non-object payload")
	}
	if _, err := parseCodexUsage([]byte(`{"rate_limit":{"allowed":true}}`)); err == nil {
		t.Fatal("want an error when no window carries usage")
	}
}

// A relative reset is anchored to the observation, not to the moment it is read.
func TestQuotaSnapshotResetAnchorsToObservation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	q := QuotaState{ObservedAt: now, Signals: map[string]string{
		codexSignalPrimary + "Used-Percent":        "50",
		codexSignalPrimary + "Reset-After-Seconds": "600",
	}}
	snap := codexSnapshotOf(q, now.Add(4*time.Minute))
	if snap == nil {
		t.Fatal("snapshot is nil for a fresh observation")
	}
	if !snap.primaryWindow.resetAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("reset = %v, want observation+10m", snap.primaryWindow.resetAt)
	}
}

// A passed primary reset must not hide a live exhausted secondary window.
func TestQuotaAwareLiveExhaustionSurvivesPassedReset(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	auths := []*Auth{observed(quotaTestAuths("a")[0], now, map[string]string{
		codexSignalAllowed:                    "true",
		codexSignalPrimary + "Used-Percent":   "50",
		codexSignalPrimary + "Reset-At":       strconv.FormatInt(now.Add(time.Minute).Unix(), 10),
		codexSignalSecondary + "Used-Percent": "100",
		codexSignalSecondary + "Reset-At":     strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
	})}
	clock.Advance(2 * time.Minute)

	if picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err == nil {
		t.Fatalf("Pick() = %v; a weekly window at 100%% must stay exhausted after the primary reset passed", picked.ID)
	}
}

// Probes must use the host proxy-url when the auth has no proxy override, as
// the executors do, and an auth override must still win.
func TestQuotaAwareProbesUseHostProxyWithoutAuthOverride(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	probed := make(chan codexUsageCredential, 2)
	s := newQuotaTestSelector(clock, func(_ context.Context, cred codexUsageCredential) (http.Header, error) {
		probed <- cred
		return nil, errors.New("probe skipped")
	})
	s.hostProxyURL = "http://host-proxy.example:3128"
	override := quotaTestAuths("b")[0]
	override.ProxyURL = "http://auth-proxy.example:8080"
	auths := append(quotaTestAuths("a"), override)
	if _, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	s.refreshDue(context.Background())

	got := map[string]string{}
	for range auths {
		select {
		case cred := <-probed:
			got[cred.accountID] = cred.proxyURL
		case <-time.After(10 * time.Second):
			t.Fatal("probe did not start")
		}
	}
	if got["acct-a"] != "http://host-proxy.example:3128" {
		t.Fatalf("auth without override probed via %q, want host proxy", got["acct-a"])
	}
	if got["acct-b"] != "http://auth-proxy.example:8080" {
		t.Fatalf("auth with override probed via %q, want its own proxy", got["acct-b"])
	}
}

// Sole-window reset: flags from the same window expire with it, so the credential is unknown.
func TestQuotaAwareExpiredFlagsBecomeUnknown(t *testing.T) {
	for _, flags := range []map[string]string{
		{codexSignalAllowed: "false"},
		{codexSignalLimitReached: "true"},
	} {
		clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
		s := newQuotaTestSelector(clock, nil)
		now := clock.Now()
		signals := usageSignals(100, now.Add(time.Minute))
		for k, v := range flags {
			signals[k] = v
		}
		auths := []*Auth{observed(quotaTestAuths("a")[0], now, signals)}
		clock.Advance(2 * time.Minute)

		picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil || picked.ID != "a" {
			t.Fatalf("flags %v: Pick() = %v, %v; want a once its window reset", flags, picked, err)
		}
	}
}

// A passed window's flag must not hide a live exhausted window, and a live
// window at a known usage level does not justify keeping the expired flag.
func TestQuotaAwareExpiredFlagKeepsLiveExhaustion(t *testing.T) {
	clock := &quotaTestClock{now: time.Unix(1_800_000_000, 0)}
	s := newQuotaTestSelector(clock, nil)
	now := clock.Now()
	spent := map[string]string{
		codexSignalAllowed:                    "false",
		codexSignalLimitReached:               "true",
		codexSignalPrimary + "Used-Percent":   "50",
		codexSignalPrimary + "Reset-At":       strconv.FormatInt(now.Add(time.Minute).Unix(), 10),
		codexSignalSecondary + "Used-Percent": "100",
		codexSignalSecondary + "Reset-At":     strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
	}
	steady := map[string]string{
		codexSignalAllowed:                    "false",
		codexSignalPrimary + "Used-Percent":   "50",
		codexSignalPrimary + "Reset-At":       strconv.FormatInt(now.Add(time.Minute).Unix(), 10),
		codexSignalSecondary + "Used-Percent": "50",
		codexSignalSecondary + "Reset-At":     strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
	}
	spentAuths := []*Auth{observed(quotaTestAuths("spent")[0], now, spent)}
	steadyAuths := []*Auth{observed(quotaTestAuths("steady")[0], now, steady)}
	clock.Advance(2 * time.Minute)

	if picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, spentAuths); err == nil {
		t.Fatalf("Pick() = %v; a live exhausted secondary window must keep the credential out", picked.ID)
	}
	if picked, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, steadyAuths); err != nil || picked.ID != "steady" {
		t.Fatalf("Pick() = %v, %v; want the expired flag dropped when no live window is exhausted", picked, err)
	}
}

// quotaTestExecutor counts calls for a provider identifier.
type quotaTestExecutor struct {
	id    string
	calls atomic.Int32
}

func (e *quotaTestExecutor) Identifier() string { return e.id }

func (e *quotaTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *quotaTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("stream not supported")
}

func (e *quotaTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *quotaTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not supported")
}

func (e *quotaTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not supported")
}

func quotaCodexAuth(id string, quota QuotaState) *Auth {
	return &Auth{
		ID:       id,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "tok-" + id, "account_id": "acct-" + id},
		Quota:    quota,
	}
}

// quotaExecuteManager returns a manager that selects with s and serves the auths
// under model. Probes stay manual, so no network call can happen in these tests.
func quotaExecuteManager(t *testing.T, s *QuotaAwareSelector, model string, executors []*quotaTestExecutor, auths []*Auth) *Manager {
	t.Helper()
	m := NewManager(nil, s, nil)
	for _, e := range executors {
		m.RegisterExecutor(e)
	}
	for _, a := range auths {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatalf("Register(%s): %v", a.ID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: model}})
		id := a.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return m
}

// Execute always selects with provider "mixed". A freshly exhausted Codex singleton
// must be rejected there, not served by round-robin.
func TestManagerExecuteRejectsExhaustedCodexSingleton(t *testing.T) {
	const model = "quota-test-singleton"
	now := time.Now()
	s := NewQuotaAwareSelector()
	s.manualRefresh = true
	codex := &quotaTestExecutor{id: "codex"}
	m := quotaExecuteManager(t, s, model, []*quotaTestExecutor{codex}, []*Auth{
		quotaCodexAuth("codex-spent", QuotaState{ObservedAt: now, Signals: usageSignals(100, now.Add(time.Hour))}),
	})

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("Execute served an exhausted Codex credential")
	}
	if got := codex.calls.Load(); got != 0 {
		t.Fatalf("exhausted credential reached the executor %d times", got)
	}
}

// In a mixed pool, an exhausted Codex credential is skipped and the other provider serves.
func TestManagerExecuteSkipsExhaustedCodexInMixedPool(t *testing.T) {
	const model = "quota-test-mixed"
	now := time.Now()
	s := NewQuotaAwareSelector()
	s.manualRefresh = true
	codex := &quotaTestExecutor{id: "codex"}
	other := &quotaTestExecutor{id: "quota-test-other"}
	m := quotaExecuteManager(t, s, model, []*quotaTestExecutor{codex, other}, []*Auth{
		quotaCodexAuth("a-codex-spent", QuotaState{ObservedAt: now, Signals: usageSignals(100, now.Add(time.Hour))}),
		{ID: "b-other", Provider: "quota-test-other", Status: StatusActive},
	})

	resp, err := m.Execute(context.Background(), []string{"codex", "quota-test-other"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if codex.calls.Load() != 0 || other.calls.Load() != 1 {
		t.Fatalf("calls codex=%d other=%d, want 0 and 1", codex.calls.Load(), other.calls.Load())
	}
}

// A Codex credential with no observation is still served through Execute (not starved).
func TestManagerExecuteServesUnobservedCodexSingleton(t *testing.T) {
	const model = "quota-test-unobserved"
	s := NewQuotaAwareSelector()
	s.manualRefresh = true
	codex := &quotaTestExecutor{id: "codex"}
	m := quotaExecuteManager(t, s, model, []*quotaTestExecutor{codex}, []*Auth{quotaCodexAuth("codex-new", QuotaState{})})

	if _, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := codex.calls.Load(); got != 1 {
		t.Fatalf("executor calls = %d, want 1", got)
	}
}
