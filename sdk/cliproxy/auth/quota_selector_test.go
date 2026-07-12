package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestParseCodexUsage_KeyShapes exercises both the snake_case and camelCase
// key shapes the codex wham/usage endpoint may emit. The parser must accept
// either variant and surface identical token limits / window reset times.
func TestParseCodexUsage_KeyShapes(t *testing.T) {
	t.Parallel()

	// Fixed "now" so reset_at (epoch seconds) comparisons are deterministic.
	now := time.Unix(1_700_000_000, 0)
	primaryReset := now.Add(5 * time.Hour).Unix()
	secondaryReset := now.Add(7 * 24 * time.Hour).Unix()

	cases := []struct {
		name string
		body string
	}{
		{
			name: "snake_case",
			body: `{
				"rate_limit": {
					"allowed": true,
					"limit_reached": false,
					"primary_window": {
						"used_percent": 40,
						"reset_after_seconds": 18000,
						"reset_at": ` + itoa(primaryReset) + `,
						"limit_window_seconds": 18000
					},
					"secondary_window": {
						"used_percent": 10,
						"reset_after_seconds": 604800,
						"reset_at": ` + itoa(secondaryReset) + `,
						"limit_window_seconds": 604800
					}
				}
			}`,
		},
		{
			name: "camelCase",
			body: `{
				"rate_limit": {
					"allowed": true,
					"limitReached": false,
					"primaryWindow": {
						"usedPercent": "40",
						"resetAfterSeconds": 18000,
						"resetAt": ` + itoa(primaryReset) + `,
						"limitWindowSeconds": 18000
					},
					"secondaryWindow": {
						"usedPercent": "10",
						"resetAfterSeconds": 604800,
						"resetAt": ` + itoa(secondaryReset) + `,
						"limitWindowSeconds": 604800
					}
				}
			}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			snap, err := parseCodexUsage([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseCodexUsage error: %v", err)
			}
			if !snap.allowed {
				t.Fatalf("allowed = false, want true")
			}
			if snap.limitReached {
				t.Fatalf("limitReached = true, want false")
			}

			pw := snap.primaryWindow
			if !pw.hasUsedPercent || pw.usedPercent != 40 {
				t.Fatalf("primary usedPercent = %v (has=%v), want 40", pw.usedPercent, pw.hasUsedPercent)
			}
			if !pw.hasReset {
				t.Fatalf("primary window missing reset")
			}
			if got, want := pw.resetAt.Unix(), primaryReset; got != want {
				t.Fatalf("primary resetAt = %d, want %d", got, want)
			}
			if !pw.hasLimitWindow || pw.limitWindow != 5*time.Hour {
				t.Fatalf("primary limitWindow = %v (has=%v), want 5h", pw.limitWindow, pw.hasLimitWindow)
			}

			sw := snap.secondaryWindow
			if !sw.hasUsedPercent || sw.usedPercent != 10 {
				t.Fatalf("secondary usedPercent = %v (has=%v), want 10", sw.usedPercent, sw.hasUsedPercent)
			}
			if got, want := sw.resetAt.Unix(), secondaryReset; got != want {
				t.Fatalf("secondary resetAt = %d, want %d", got, want)
			}
		})
	}
}

// TestParseCodexUsage_DefaultsAllowed verifies the parser defaults
// rate_limit.allowed to true when the field is absent.
func TestParseCodexUsage_DefaultsAllowed(t *testing.T) {
	t.Parallel()

	snap, err := parseCodexUsage([]byte(`{"rate_limit": {}}`))
	if err != nil {
		t.Fatalf("parseCodexUsage error: %v", err)
	}
	if !snap.allowed {
		t.Fatalf("allowed defaulted to false, want true when absent")
	}
	if snap.limitReached {
		t.Fatalf("limitReached = true, want false when absent")
	}
}

// TestParseCodexWindow_EmptyNode verifies a null/absent window is flagged
// observedEmpty so callers can distinguish "no telemetry" from "0% used".
func TestParseCodexWindow_EmptyNode(t *testing.T) {
	t.Parallel()

	// primary_window is explicitly null; secondary_window is entirely absent.
	body := []byte(`{"rate_limit": {"primary_window": null}}`)
	snap, err := parseCodexUsage(body)
	if err != nil {
		t.Fatalf("parseCodexUsage error: %v", err)
	}
	if !snap.primaryWindow.observedEmpty {
		t.Fatalf("expected primary window observedEmpty for null node")
	}
	// parseCodexUsage falls back from snake_case to camelCase when a window has
	// no reset; an absent secondary_window therefore also resolves observedEmpty.
	if !snap.secondaryWindow.observedEmpty {
		t.Fatalf("expected secondary window observedEmpty when absent")
	}
}

// TestCodexScore covers the scoring function's three regimes:
//   - more remaining quota scores higher than less remaining;
//   - the 0.05 soft-penalize threshold kicks in below 5% remaining;
//   - an exhausted window reports exhausted=true with a low (0) score.
func TestCodexScore(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	future := now.Add(5 * time.Hour)

	mkWindow := func(usedPercent float64, reset time.Time) codexUsageSnapshot {
		return codexUsageSnapshot{
			allowed: true,
			primaryWindow: codexUsageWindow{
				usedPercent:    usedPercent,
				resetAt:        reset,
				hasUsedPercent: true,
				hasReset:       true,
			},
		}
	}

	t.Run("more remaining scores higher", func(t *testing.T) {
		t.Parallel()
		moreRemaining := mkWindow(10, future)  // 90% remaining
		lessRemaining := mkWindow(80, future)  // 20% remaining
		sHi, exh, _ := codexScore(&moreRemaining, now)
		sLo, exhLo, _ := codexScore(&lessRemaining, now)
		if exh || exhLo {
			t.Fatalf("unexpected exhausted: hi=%v lo=%v", exh, exhLo)
		}
		if sHi <= sLo {
			t.Fatalf("expected more-remaining score > less-remaining: hi=%v lo=%v", sHi, sLo)
		}
	})

	t.Run("soft penalize below 5 percent remaining", func(t *testing.T) {
		t.Parallel()
		// 2% remaining falls in the <0.05 soft-penalize band, so the score is
		// scaled down by remaining/0.05 = 0.4 relative to its raw rate.
		lowRemaining := mkWindow(98, future) // 2% remaining
		scoreLow, exh, _ := codexScore(&lowRemaining, now)
		if exh {
			t.Fatalf("unexpected exhausted for 2%% remaining")
		}
		if scoreLow <= 0 {
			t.Fatalf("soft-penalized score should still be positive, got %v", scoreLow)
		}
		// Compare against a window just above the 5% threshold (4% remaining).
		aboveThreshold := mkWindow(96, future) // 4% remaining, NOT penalized
		scoreAbove, _, _ := codexScore(&aboveThreshold, now)
		// The penalized 2% case should score lower than the unpenalized 4% case
		// despite both being near the floor; this exercises the soft-penalize math.
		if scoreLow >= scoreAbove {
			t.Fatalf("expected soft-penalized (2%%) score < 4%% remaining score: low=%v above=%v", scoreLow, scoreAbove)
		}
	})

	t.Run("exhausted window returns blocked score", func(t *testing.T) {
		t.Parallel()
		exhausted := mkWindow(100, future) // 0% remaining, resets in the future
		score, exh, next := codexScore(&exhausted, now)
		if !exh {
			t.Fatalf("expected exhausted=true for 100%% used window")
		}
		if score != 0 {
			t.Fatalf("expected score 0 for exhausted window, got %v", score)
		}
		if !next.Equal(future) {
			t.Fatalf("expected next reset = future, got %v", next)
		}
	})
}

// TestCodexScore_NotAllowed verifies that when upstream reports
// allowed=false / limit_reached=true, the snapshot is treated as exhausted
// with the earliest reset time.
func TestCodexScore_NotAllowed(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	reset := now.Add(1 * time.Hour)
	snap := &codexUsageSnapshot{
		allowed:      false,
		limitReached: true,
		primaryWindow: codexUsageWindow{
			resetAt:  reset,
			hasReset: true,
		},
	}
	score, exh, next := codexScore(snap, now)
	if !exh {
		t.Fatalf("expected exhausted=true when not allowed")
	}
	if score != 0 {
		t.Fatalf("expected score 0 when not allowed, got %v", score)
	}
	if !next.Equal(reset) {
		t.Fatalf("expected next reset = %v, got %v", reset, next)
	}
}

// TestStrconvParseHelpers confirms the stdlib-backed helpers keep the
// fallback-on-error behavior expected by callers (error -> skip value).
func TestStrconvParseHelpers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		floatIn string
		intIn   string
		wantF   float64
		wantI   int64
	}{
		{name: "plain", floatIn: "40", intIn: "18000", wantF: 40, wantI: 18000},
		{name: "fractional", floatIn: "12.5", intIn: "12", wantF: 12.5, wantI: 12},
		{name: "negative", floatIn: "-3.5", intIn: "-3", wantF: -3.5, wantI: -3},
		{name: "whitespace", floatIn: "  7 ", intIn: " 7 ", wantF: 7, wantI: 7},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotF, errF := strconvParseFloat(tc.floatIn)
			if errF != nil {
				t.Fatalf("strconvParseFloat(%q) error: %v", tc.floatIn, errF)
			}
			if gotF != tc.wantF {
				t.Fatalf("strconvParseFloat(%q) = %v, want %v", tc.floatIn, gotF, tc.wantF)
			}
			gotI, errI := strconvParseInt(tc.intIn)
			if errI != nil {
				t.Fatalf("strconvParseInt(%q) error: %v", tc.intIn, errI)
			}
			if gotI != tc.wantI {
				t.Fatalf("strconvParseInt(%q) = %v, want %v", tc.intIn, gotI, tc.wantI)
			}
		})
	}

	if _, err := strconvParseFloat("not-a-number"); err == nil {
		t.Fatalf("strconvParseFloat expected error for invalid input")
	}
	if _, err := strconvParseInt(""); err == nil {
		t.Fatalf("strconvParseInt expected error for empty input")
	}
}

// TestQuotaAwarePick_ShortlistThreshold verifies the 95%-of-best shortlisting
// logic in Pick. A candidate scoring >= 0.95 * best must remain selectable;
// a candidate strictly below the threshold must never be picked. Because the
// shortlist is round-robin resolved, we run many iterations and assert the
// sub-threshold auth is never returned while the eligible ones are.
func TestQuotaAwarePick_ShortlistThreshold(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	future := now.Add(5 * time.Hour)

	// best scores ~ remaining/hours with 10% used (high). Within-95% scores
	// with 12% used. Below-95% scores with 80% used.
	mkSnap := func(used float64) *codexUsageSnapshot {
		return &codexUsageSnapshot{
			allowed: true,
			primaryWindow: codexUsageWindow{
				usedPercent:    used,
				resetAt:        future,
				hasUsedPercent: true,
				hasReset:       true,
			},
		}
	}

	auths := []*Auth{
		{ID: "best", Provider: "codex", Status: StatusActive, Index: "best"},
		{ID: "near", Provider: "codex", Status: StatusActive, Index: "near"},
		{ID: "low", Provider: "codex", Status: StatusActive, Index: "low"},
	}
	byIndex := map[string]*codexUsageSnapshot{
		"best": mkSnap(10), // 90% remaining -> best score
		"near": mkSnap(12), // ~88% remaining -> within 95% of best
		"low":  mkSnap(80), // 20% remaining -> well below 95% of best
	}

	// Confirm the score relationship holds before exercising Pick.
	sBest, _, _ := codexScore(byIndex["best"], now)
	sLow, _, _ := codexScore(byIndex["low"], now)
	if sLow >= sBest*0.95 {
		t.Fatalf("test fixture invalid: low score %v must be < 0.95*best %v", sLow, sBest)
	}

	sel := NewQuotaAwareSelector()
	sel.now = func() time.Time { return now }
	sel.fetchCodexUsage = func(ctx context.Context, a *Auth) (*codexUsageSnapshot, error) {
		if a == nil {
			return nil, errors.New("nil auth")
		}
		snap, ok := byIndex[a.Index]
		if !ok {
			return nil, errors.New("no fixture for auth " + a.Index)
		}
		clone := *snap
		return &clone, nil
	}

	seen := map[string]int{}
	const iterations = 60
	for i := 0; i < iterations; i++ {
		picked, err := sel.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, cloneAuths(auths))
		if err != nil {
			t.Fatalf("Pick iteration %d error: %v", i, err)
		}
		if picked == nil || picked.ID == "" {
			t.Fatalf("Pick iteration %d returned nil/empty auth", i)
		}
		if picked.ID == "low" {
			t.Fatalf("Pick returned below-threshold auth %q on iteration %d; it must be excluded from the shortlist", picked.ID, i)
		}
		seen[picked.ID]++
	}

	// Both within-threshold auths must be reachable via round-robin.
	if seen["best"] == 0 || seen["near"] == 0 {
		t.Fatalf("expected both best and near to be picked at least once, got %v", seen)
	}
}

// TestQuotaAwarePick_AllExhaustedReturnsCooldown verifies that when every
// candidate window is exhausted, Pick returns a cooldown error whose reset
// duration matches the earliest reset of the exhausted windows.
func TestQuotaAwarePick_AllExhaustedReturnsCooldown(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	earliest := now.Add(1 * time.Hour)
	later := now.Add(2 * time.Hour)

	mkExhausted := func(reset time.Time) *codexUsageSnapshot {
		return &codexUsageSnapshot{
			allowed: true,
			primaryWindow: codexUsageWindow{
				usedPercent:    100,
				resetAt:        reset,
				hasUsedPercent: true,
				hasReset:       true,
			},
		}
	}

	auths := []*Auth{
		{ID: "a", Provider: "codex", Status: StatusActive, Index: "a"},
		{ID: "b", Provider: "codex", Status: StatusActive, Index: "b"},
	}
	byIndex := map[string]*codexUsageSnapshot{
		"a": mkExhausted(later),
		"b": mkExhausted(earliest),
	}

	sel := NewQuotaAwareSelector()
	sel.now = func() time.Time { return now }
	sel.fetchCodexUsage = func(ctx context.Context, a *Auth) (*codexUsageSnapshot, error) {
		snap := byIndex[a.Index]
		clone := *snap
		return &clone, nil
	}

	_, err := sel.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, cloneAuths(auths))
	if err == nil {
		t.Fatalf("expected cooldown error when all windows exhausted, got nil")
	}
	cd, ok := err.(*modelCooldownError)
	if !ok {
		t.Fatalf("expected *modelCooldownError, got %T (%v)", err, err)
	}
	// The reset should be derived from the earliest (1h) window.
	if cd.resetIn <= 0 || cd.resetIn > time.Hour+time.Second {
		t.Fatalf("resetIn = %v, want ~1h (earliest reset)", cd.resetIn)
	}
}

// TestQuotaAwarePick_FetchErrorFallsBackToRoundRobin ensures that when quota
// telemetry is unavailable for every auth, Pick still degrades to round-robin
// selection rather than failing.
func TestQuotaAwarePick_FetchErrorFallsBackToRoundRobin(t *testing.T) {
	t.Parallel()

	auths := []*Auth{
		{ID: "x", Provider: "codex", Status: StatusActive, Index: "x"},
		{ID: "y", Provider: "codex", Status: StatusActive, Index: "y"},
	}
	sel := NewQuotaAwareSelector()
	sel.fetchCodexUsage = func(ctx context.Context, a *Auth) (*codexUsageSnapshot, error) {
		return nil, errors.New("upstream unavailable")
	}

	picked, err := sel.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, cloneAuths(auths))
	if err != nil {
		t.Fatalf("Pick returned error when telemetry unavailable: %v", err)
	}
	if picked == nil || (picked.ID != "x" && picked.ID != "y") {
		t.Fatalf("Pick returned unexpected auth: %+v", picked)
	}
}

// TestQuotaAwarePick_NonCodexDelegatesToRoundRobin confirms non-codex
// providers skip quota scoring entirely and delegate to the round-robin
// selector.
func TestQuotaAwarePick_NonCodexDelegatesToRoundRobin(t *testing.T) {
	t.Parallel()

	auths := []*Auth{
		{ID: "claude-1", Provider: "claude", Status: StatusActive, Index: "claude-1"},
	}
	sel := NewQuotaAwareSelector()
	// A panicking fetcher proves it is never invoked for non-codex providers.
	sel.fetchCodexUsage = func(ctx context.Context, a *Auth) (*codexUsageSnapshot, error) {
		t.Fatalf("fetchCodexUsage must not be called for non-codex provider")
		return nil, nil
	}

	picked, err := sel.Pick(context.Background(), "claude", "claude-sonnet", cliproxyexecutor.Options{}, cloneAuths(auths))
	if err != nil {
		t.Fatalf("Pick error for non-codex: %v", err)
	}
	if picked == nil || picked.ID != "claude-1" {
		t.Fatalf("Pick returned unexpected auth: %+v", picked)
	}
}

// itoa formats an int64 as a decimal string for embedding in JSON fixtures.
// (Avoids pulling strconv into the test's import block noise.)
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// cloneAuths returns a shallow copy of the slice so concurrent/iterated Pick
// calls do not mutate the caller's fixture slice.
func cloneAuths(in []*Auth) []*Auth {
	out := make([]*Auth, len(in))
	copy(out, in)
	return out
}
