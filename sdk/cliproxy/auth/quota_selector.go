package auth

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// QuotaAwareSelector balances usage across credentials using provider-specific quota telemetry.
//
// Today this selector is implemented for Codex only, using the same usage endpoint the
// Management UI reads (https://chatgpt.com/backend-api/wham/usage). For other providers it
// falls back to round-robin.
//
// Selection intent:
// - Avoid exhausting "short window" (e.g. 5-hour) limits too early.
// - Avoid exhausting "long window" (e.g. weekly) limits too early.
// - Prefer accounts that can "afford" to spend quota faster (more remaining, or sooner reset).
// - Keep operator overrides: any auth priority buckets are still respected.
type QuotaAwareSelector struct {
	rr RoundRobinSelector

	mu sync.Mutex

	cacheTTL time.Duration
	now      func() time.Time

	codexCache map[string]codexQuotaCacheEntry

	fetchCodexUsage func(context.Context, *Auth) (*codexUsageSnapshot, error)
}

type codexQuotaCacheEntry struct {
	fetchedAt time.Time
	snapshot  codexUsageSnapshot
}

type codexUsageSnapshot struct {
	allowed         bool
	limitReached    bool
	primaryWindow   codexUsageWindow
	secondaryWindow codexUsageWindow
}

type codexUsageWindow struct {
	usedPercent    float64
	resetAfter     time.Duration
	resetAt        time.Time
	hasUsedPercent bool
	hasReset       bool
	limitWindow    time.Duration
	hasLimitWindow bool
	observedEmpty  bool
}

func NewQuotaAwareSelector() *QuotaAwareSelector {
	return &QuotaAwareSelector{
		cacheTTL:        60 * time.Second,
		now:             time.Now,
		codexCache:      make(map[string]codexQuotaCacheEntry),
		fetchCodexUsage: fetchCodexUsageDefault,
	}
}

func (s *QuotaAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if provider != "codex" {
		return s.rr.Pick(ctx, provider, model, opts, auths)
	}

	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}

	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	if len(available) <= 1 {
		if len(available) == 0 {
			return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
		}
		return available[0], nil
	}

	type scored struct {
		auth *Auth
		ok   bool
		s    float64
		next time.Time
	}
	scores := make([]scored, 0, len(available))
	cooldownCount := 0
	var earliest time.Time

	for _, a := range available {
		snapshot, snapOK := s.getCodexUsage(ctx, a, now)
		if !snapOK {
			scores = append(scores, scored{auth: a, ok: false})
			continue
		}

		score, exhausted, next := codexScore(snapshot, now)
		if exhausted {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
			// Do not add exhausted auth to scoring set.
			continue
		}

		scores = append(scores, scored{auth: a, ok: true, s: score})
	}

	if len(scores) == 0 && cooldownCount == len(available) && !earliest.IsZero() {
		resetIn := earliest.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return nil, newModelCooldownError(model, "", resetIn)
	}

	eligible := make([]*Auth, 0, len(scores))
	best := -1.0
	for _, v := range scores {
		// If we have no quota telemetry for an auth, keep it eligible but treat its score as 0.
		score := 0.0
		if v.ok {
			score = v.s
		}
		if score < 0 {
			continue
		}
		if score > best {
			best = score
		}
		eligible = append(eligible, v.auth)
	}
	if len(eligible) == 0 {
		return s.rr.Pick(ctx, provider, model, opts, available)
	}

	// Prefer candidates within 95% of the best score to avoid sticking to a single account.
	threshold := best * 0.95
	if best == 0 {
		threshold = 0
	}
	shortlist := make([]*Auth, 0, len(scores))
	for _, v := range scores {
		if v.auth == nil {
			continue
		}
		score := 0.0
		if v.ok {
			score = v.s
		}
		if score >= threshold {
			shortlist = append(shortlist, v.auth)
		}
	}
	if len(shortlist) == 0 {
		shortlist = eligible
	}
	if len(shortlist) == 1 {
		return shortlist[0], nil
	}
	return s.rr.Pick(ctx, provider, model, opts, shortlist)
}

func (s *QuotaAwareSelector) getCodexUsage(ctx context.Context, auth *Auth, now time.Time) (*codexUsageSnapshot, bool) {
	if s == nil || auth == nil {
		return nil, false
	}
	auth.EnsureIndex()
	key := auth.Index
	if key == "" {
		key = auth.ID
	}
	if key == "" {
		return nil, false
	}

	ttl := s.cacheTTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}

	s.mu.Lock()
	entry, ok := s.codexCache[key]
	if ok && !entry.fetchedAt.IsZero() && now.Sub(entry.fetchedAt) <= ttl {
		snap := entry.snapshot
		s.mu.Unlock()
		return &snap, true
	}
	fetcher := s.fetchCodexUsage
	s.mu.Unlock()

	if fetcher == nil {
		return nil, false
	}

	fetchCtx := ctx
	if fetchCtx == nil {
		fetchCtx = context.Background()
	}
	timeout := 2 * time.Second
	if deadline, okDeadline := fetchCtx.Deadline(); okDeadline {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	var cancel context.CancelFunc
	fetchCtx, cancel = context.WithTimeout(fetchCtx, timeout)
	defer cancel()

	snapshot, err := fetcher(fetchCtx, auth)
	if err != nil || snapshot == nil {
		// return stale entry if we have one
		if ok {
			snap := entry.snapshot
			return &snap, true
		}
		return nil, false
	}

	s.mu.Lock()
	if s.codexCache == nil {
		s.codexCache = make(map[string]codexQuotaCacheEntry)
	}
	s.codexCache[key] = codexQuotaCacheEntry{fetchedAt: now, snapshot: *snapshot}
	s.mu.Unlock()
	return snapshot, true
}

func codexScore(snapshot *codexUsageSnapshot, now time.Time) (score float64, exhausted bool, next time.Time) {
	if snapshot == nil {
		return -1, false, time.Time{}
	}
	if !snapshot.allowed || snapshot.limitReached {
		// Treat as exhausted when upstream says no more requests are allowed.
		next = minReset(now, snapshot.primaryWindow, snapshot.secondaryWindow)
		if !next.IsZero() && next.After(now) {
			return 0, true, next
		}
	}

	score = math.Inf(1)
	applyWindow := func(window codexUsageWindow) {
		if !window.hasReset || window.resetAt.IsZero() {
			return
		}
		if window.hasUsedPercent {
			remaining := clamp01(1.0 - (window.usedPercent / 100.0))
			if remaining <= 0 && window.resetAt.After(now) {
				exhausted = true
				if next.IsZero() || window.resetAt.Before(next) {
					next = window.resetAt
				}
				return
			}
			hours := window.resetAt.Sub(now).Hours()
			if hours < 0 {
				hours = 0
			}
			// Avoid spikes when a reset is very close.
			hours = math.Max(hours, 0.25)
			rate := remaining / hours
			// Soft-penalize when near exhaustion to keep a safety buffer.
			if remaining < 0.05 {
				rate *= remaining / 0.05
			}
			if rate < score {
				score = rate
			}
		}
	}

	applyWindow(snapshot.primaryWindow)
	if exhausted {
		return 0, true, next
	}
	applyWindow(snapshot.secondaryWindow)
	if exhausted {
		return 0, true, next
	}

	if math.IsInf(score, 1) {
		return -1, false, time.Time{}
	}
	return score, false, time.Time{}
}

func minReset(now time.Time, windows ...codexUsageWindow) time.Time {
	var earliest time.Time
	for _, w := range windows {
		if !w.hasReset || w.resetAt.IsZero() {
			continue
		}
		if w.resetAt.Before(now) {
			continue
		}
		if earliest.IsZero() || w.resetAt.Before(earliest) {
			earliest = w.resetAt
		}
	}
	return earliest
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func fetchCodexUsageDefault(ctx context.Context, auth *Auth) (*codexUsageSnapshot, error) {
	if auth == nil {
		return nil, fmt.Errorf("codex usage: auth is nil")
	}
	var token string
	if auth.Attributes != nil {
		token = strings.TrimSpace(auth.Attributes["api_key"])
	}
	if token == "" && auth.Metadata != nil {
		if v, ok := auth.Metadata["access_token"].(string); ok {
			token = strings.TrimSpace(v)
		}
	}
	if token == "" {
		return nil, fmt.Errorf("codex usage: missing access token")
	}
	accountID := ""
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["account_id"].(string); ok {
			accountID = strings.TrimSpace(v)
		}
	}
	if accountID == "" {
		return nil, fmt.Errorf("codex usage: missing account_id")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Chatgpt-Account-Id", accountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex usage: status %d", resp.StatusCode)
	}
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 512*1024 {
				break
			}
		}
		if readErr != nil {
			break
		}
	}
	return parseCodexUsage(buf)
}

func parseCodexUsage(body []byte) (*codexUsageSnapshot, error) {
	root := gjson.ParseBytes(body)
	if !root.Exists() {
		return nil, fmt.Errorf("codex usage: empty payload")
	}
	snap := &codexUsageSnapshot{}

	allowed := root.Get("rate_limit.allowed")
	if allowed.Exists() {
		snap.allowed = allowed.Bool()
	} else {
		snap.allowed = true
	}
	limitReached := root.Get("rate_limit.limit_reached")
	if !limitReached.Exists() {
		limitReached = root.Get("rate_limit.limitReached")
	}
	if limitReached.Exists() {
		snap.limitReached = limitReached.Bool()
	}

	snap.primaryWindow = parseCodexWindow(root.Get("rate_limit.primary_window"))
	if !snap.primaryWindow.hasReset {
		snap.primaryWindow = parseCodexWindow(root.Get("rate_limit.primaryWindow"))
	}
	snap.secondaryWindow = parseCodexWindow(root.Get("rate_limit.secondary_window"))
	if !snap.secondaryWindow.hasReset {
		snap.secondaryWindow = parseCodexWindow(root.Get("rate_limit.secondaryWindow"))
	}
	return snap, nil
}

func parseCodexWindow(node gjson.Result) codexUsageWindow {
	window := codexUsageWindow{}
	if !node.Exists() || node.Type == gjson.Null {
		window.observedEmpty = true
		return window
	}

	used := node.Get("used_percent")
	if !used.Exists() {
		used = node.Get("usedPercent")
	}
	if used.Exists() {
		if used.Type == gjson.String {
			if f, err := strconvParseFloat(used.String()); err == nil {
				window.usedPercent = f
				window.hasUsedPercent = true
			}
		} else if used.Type == gjson.Number {
			window.usedPercent = used.Float()
			window.hasUsedPercent = true
		}
	}

	resetAfter := node.Get("reset_after_seconds")
	if !resetAfter.Exists() {
		resetAfter = node.Get("resetAfterSeconds")
	}
	if resetAfter.Exists() {
		sec := int64(resetAfter.Int())
		if sec > 0 {
			window.resetAfter = time.Duration(sec) * time.Second
		}
	}

	resetAt := node.Get("reset_at")
	if !resetAt.Exists() {
		resetAt = node.Get("resetAt")
	}
	if resetAt.Exists() {
		// reset_at appears to be seconds since epoch in Codex usage payload.
		sec := int64(resetAt.Int())
		if sec > 0 {
			window.resetAt = time.Unix(sec, 0)
			window.hasReset = true
		}
	}

	limitWindow := node.Get("limit_window_seconds")
	if !limitWindow.Exists() {
		limitWindow = node.Get("limitWindowSeconds")
	}
	if limitWindow.Exists() {
		sec := int64(limitWindow.Int())
		if sec > 0 {
			window.limitWindow = time.Duration(sec) * time.Second
			window.hasLimitWindow = true
		}
	}

	if !window.hasReset && window.resetAfter > 0 {
		window.resetAt = time.Now().Add(window.resetAfter)
		window.hasReset = true
	}

	return window
}

func strconvParseFloat(v string) (float64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("empty")
	}
	var sign float64 = 1
	if strings.HasPrefix(v, "-") {
		sign = -1
		v = strings.TrimPrefix(v, "-")
	}
	// only allow a simple float form
	dot := strings.IndexByte(v, '.')
	if dot < 0 {
		i, err := strconvParseInt(v)
		if err != nil {
			return 0, err
		}
		return sign * float64(i), nil
	}
	intPart := v[:dot]
	fracPart := v[dot+1:]
	ii, err := strconvParseInt(intPart)
	if err != nil {
		return 0, err
	}
	ff, err := strconvParseInt(fracPart)
	if err != nil {
		return 0, err
	}
	div := math.Pow10(len(fracPart))
	return sign * (float64(ii) + float64(ff)/div), nil
}

func strconvParseInt(v string) (int64, error) {
	if v == "" {
		return 0, fmt.Errorf("empty")
	}
	var n int64
	for i := 0; i < len(v); i++ {
		ch := v[i]
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid digit")
		}
		n = n*10 + int64(ch-'0')
	}
	return n, nil
}
