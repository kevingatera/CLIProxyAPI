package auth

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

const (
	codexUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"
	// codexCLIUserAgent makes the usage endpoint answer with the shape it returns to the codex CLI.
	codexCLIUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"

	quotaRefreshInterval   = 60 * time.Second
	quotaMaxStaleness      = 5 * time.Minute
	quotaRefreshTick       = 15 * time.Second
	quotaRefreshBackoff    = 30 * time.Second
	quotaRefreshBackoffMax = 10 * time.Minute
	quotaMaxConcurrent     = 4
	quotaTrackedTTL        = 30 * time.Minute
	quotaMaxTracked        = 4096
	quotaShortlistRatio    = 0.95
	quotaUsageBodyLimit    = 512 * 1024
)

// QuotaAwareSelector ranks Codex credentials by remaining usage window and
// round-robins among the best of them. Other providers use round-robin.
//
// Pick never performs I/O. It reads cached usage snapshots and records the
// Codex credentials it saw; a background loop probes the usage endpoint for
// those credentials. Each candidate is in exactly one state:
//   - exhausted: a fresh snapshot says the limit is reached. Never picked.
//   - known: a fresh snapshot with a score. Only the best scores are shortlisted.
//   - unknown: no snapshot, a snapshot older than quotaMaxStaleness, a snapshot
//     whose reset time has passed, or one without usable windows. Always
//     shortlisted next to the best known candidates, so it is neither starved
//     nor preferred over them.
type QuotaAwareSelector struct {
	rr RoundRobinSelector

	now   func() time.Time
	fetch func(context.Context, codexUsageCredential, time.Time) (*codexUsageSnapshot, error)
	// manualRefresh keeps Pick from starting the background loop (tests drive refreshDue).
	manualRefresh bool

	mu       sync.Mutex
	entries  map[string]*quotaEntry
	inflight int

	startOnce sync.Once
	stopOnce  sync.Once
	stopped   bool
	cancel    context.CancelFunc
	wake      chan struct{}
	wg        sync.WaitGroup
}

type quotaEntry struct {
	cred        codexUsageCredential
	lastSeen    time.Time
	snapshot    *codexUsageSnapshot
	fetchedAt   time.Time
	failures    int
	nextAttempt time.Time
	fetching    bool
}

type codexUsageCredential struct {
	token     string
	accountID string
	proxyURL  string
}

type codexUsageSnapshot struct {
	allowed         bool
	limitReached    bool
	primaryWindow   codexUsageWindow
	secondaryWindow codexUsageWindow
}

type codexUsageWindow struct {
	usedPercent    float64
	hasUsedPercent bool
	resetAt        time.Time
}

type quotaState int

const (
	quotaUnknown quotaState = iota
	quotaKnown
	quotaExhausted
)

// NewQuotaAwareSelector returns a selector that probes Codex usage in the background.
func NewQuotaAwareSelector() *QuotaAwareSelector {
	return &QuotaAwareSelector{
		now:     time.Now,
		fetch:   fetchCodexUsage,
		entries: make(map[string]*quotaEntry),
		wake:    make(chan struct{}, 1),
	}
}

func (s *QuotaAwareSelector) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Pick selects a Codex credential from cached usage only.
func (s *QuotaAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if provider != "codex" {
		return s.rr.Pick(ctx, provider, model, opts, auths)
	}
	now := s.clock()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	if !s.manualRefresh {
		s.startOnce.Do(s.start)
	}

	type scoredAuth struct {
		auth  *Auth
		score float64
	}
	var known []scoredAuth
	var unknown []*Auth
	var earliest time.Time
	needRefresh := false

	s.mu.Lock()
	for _, a := range available {
		entry := s.trackLocked(a, now)
		var snapshot *codexUsageSnapshot
		if entry != nil {
			if entry.snapshot != nil && now.Sub(entry.fetchedAt) <= quotaMaxStaleness {
				snapshot = entry.snapshot
			}
			due := entry.snapshot == nil || now.Sub(entry.fetchedAt) >= quotaRefreshInterval
			if due && !entry.fetching && !now.Before(entry.nextAttempt) {
				needRefresh = true
			}
		}
		state, score, next := classifyCodexSnapshot(snapshot, now)
		switch state {
		case quotaExhausted:
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		case quotaKnown:
			known = append(known, scoredAuth{auth: a, score: score})
		default:
			unknown = append(unknown, a)
		}
	}
	s.mu.Unlock()
	if needRefresh {
		s.signal()
	}

	if len(known) == 0 && len(unknown) == 0 {
		if earliest.IsZero() {
			return nil, &Error{Code: "auth_unavailable", Message: "all codex credentials report an exhausted usage limit", HTTPStatus: http.StatusTooManyRequests}
		}
		return nil, newModelCooldownError(model, provider, earliest.Sub(now))
	}

	best := 0.0
	for _, k := range known {
		best = math.Max(best, k.score)
	}
	threshold := best * quotaShortlistRatio
	shortlist := make([]*Auth, 0, len(known)+len(unknown))
	for _, k := range known {
		if k.score >= threshold {
			shortlist = append(shortlist, k.auth)
		}
	}
	shortlist = append(shortlist, unknown...)
	if len(shortlist) == 1 {
		return shortlist[0], nil
	}
	sort.Slice(shortlist, func(i, j int) bool { return shortlist[i].ID < shortlist[j].ID })
	// The shortlist is already filtered against the injected clock; skip revalidation.
	if ctx == nil {
		ctx = context.Background()
	}
	return s.rr.Pick(context.WithValue(ctx, prevalidatedAuthCandidatesKey{}, true), provider, model, opts, shortlist)
}

// trackLocked records the credential so the refresher can probe it. Caller holds s.mu.
func (s *QuotaAwareSelector) trackLocked(a *Auth, now time.Time) *quotaEntry {
	if a == nil || a.ID == "" {
		return nil
	}
	if s.entries == nil {
		s.entries = make(map[string]*quotaEntry)
	}
	entry, ok := s.entries[a.ID]
	if !ok {
		if len(s.entries) >= quotaMaxTracked {
			s.pruneLocked(now)
			if len(s.entries) >= quotaMaxTracked {
				return nil
			}
		}
		entry = &quotaEntry{}
		s.entries[a.ID] = entry
	}
	entry.cred = codexCredentialOf(a)
	entry.lastSeen = now
	return entry
}

func (s *QuotaAwareSelector) pruneLocked(now time.Time) {
	for id, entry := range s.entries {
		if !entry.fetching && now.Sub(entry.lastSeen) > quotaTrackedTTL {
			delete(s.entries, id)
		}
	}
}

func (s *QuotaAwareSelector) signal() {
	if s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *QuotaAwareSelector) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go s.run(ctx)
}

func (s *QuotaAwareSelector) run(ctx context.Context) {
	ticker := time.NewTicker(quotaRefreshTick)
	defer ticker.Stop()
	for {
		s.refreshDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// Stop cancels the refresher and any in-flight usage probes.
func (s *QuotaAwareSelector) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		cancel := s.cancel
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
}

// refreshDue starts probes for credentials whose snapshot is missing or older
// than quotaRefreshInterval, skipping any still in failure backoff. It does not
// wait for them, so one slow probe cannot stall the others.
func (s *QuotaAwareSelector) refreshDue(ctx context.Context) {
	if ctx.Err() != nil || s.fetch == nil {
		return
	}
	now := s.clock()
	type job struct {
		id   string
		cred codexUsageCredential
	}
	var jobs []job
	s.mu.Lock()
	s.pruneLocked(now)
	for id, entry := range s.entries {
		if s.inflight+len(jobs) >= quotaMaxConcurrent {
			break
		}
		if entry.fetching || now.Before(entry.nextAttempt) {
			continue
		}
		if entry.snapshot != nil && now.Sub(entry.fetchedAt) < quotaRefreshInterval {
			continue
		}
		entry.fetching = true
		jobs = append(jobs, job{id: id, cred: entry.cred})
	}
	s.inflight += len(jobs)
	s.mu.Unlock()

	for _, j := range jobs {
		s.wg.Add(1)
		go func(j job) {
			defer s.wg.Done()
			snapshot, errFetch := s.fetch(ctx, j.cred, now)
			s.record(j.id, snapshot, errFetch)
		}(j)
	}
}

func (s *QuotaAwareSelector) record(id string, snapshot *codexUsageSnapshot, errFetch error) {
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight--
	entry, ok := s.entries[id]
	if !ok {
		return
	}
	entry.fetching = false
	if errFetch != nil || snapshot == nil {
		entry.failures++
		backoff := quotaRefreshBackoff << min(entry.failures-1, 8)
		entry.nextAttempt = now.Add(min(backoff, quotaRefreshBackoffMax))
		if errFetch != nil {
			log.WithField("auth", id).Debugf("quota-aware: codex usage probe failed: %v", errFetch)
		}
		return
	}
	entry.snapshot = snapshot
	entry.fetchedAt = now
	entry.failures = 0
	entry.nextAttempt = time.Time{}
}

// classifyCodexSnapshot maps a fresh snapshot (or nil) to a selection state.
// A higher score means the credential can afford to spend quota faster.
func classifyCodexSnapshot(snapshot *codexUsageSnapshot, now time.Time) (quotaState, float64, time.Time) {
	if snapshot == nil {
		return quotaUnknown, 0, time.Time{}
	}
	windows := []codexUsageWindow{snapshot.primaryWindow, snapshot.secondaryWindow}
	for _, w := range windows {
		// A passed reset makes the reported usage obsolete until the next probe.
		if !w.resetAt.IsZero() && !w.resetAt.After(now) {
			return quotaUnknown, 0, time.Time{}
		}
	}
	var next time.Time
	exhausted := !snapshot.allowed || snapshot.limitReached
	score := math.Inf(1)
	for _, w := range windows {
		if !w.hasUsedPercent || w.resetAt.IsZero() {
			continue
		}
		remaining := min(max(1-w.usedPercent/100, 0), 1)
		if remaining <= 0 {
			exhausted = true
			if next.IsZero() || w.resetAt.Before(next) {
				next = w.resetAt
			}
			continue
		}
		// Clamp so a reset a few minutes away does not dominate the ranking.
		hours := math.Max(w.resetAt.Sub(now).Hours(), 0.25)
		rate := remaining / hours
		if remaining < 0.05 {
			rate *= remaining / 0.05
		}
		score = math.Min(score, rate)
	}
	if exhausted {
		if next.IsZero() {
			for _, w := range windows {
				if !w.resetAt.IsZero() && (next.IsZero() || w.resetAt.Before(next)) {
					next = w.resetAt
				}
			}
		}
		return quotaExhausted, 0, next
	}
	if math.IsInf(score, 1) {
		return quotaUnknown, 0, time.Time{}
	}
	return quotaKnown, score, time.Time{}
}

func codexCredentialOf(a *Auth) codexUsageCredential {
	cred := codexUsageCredential{proxyURL: strings.TrimSpace(a.ProxyURL)}
	if a.Attributes != nil {
		cred.token = strings.TrimSpace(a.Attributes["api_key"])
	}
	if a.Metadata != nil {
		if cred.token == "" {
			if v, ok := a.Metadata["access_token"].(string); ok {
				cred.token = strings.TrimSpace(v)
			}
		}
		if v, ok := a.Metadata["account_id"].(string); ok {
			cred.accountID = strings.TrimSpace(v)
		}
	}
	return cred
}

// fetchCodexUsage probes wham/usage. It sets no timeout; the refresher's
// context cancels it on Stop.
func fetchCodexUsage(ctx context.Context, cred codexUsageCredential, now time.Time) (*codexUsageSnapshot, error) {
	if cred.token == "" || cred.accountID == "" {
		return nil, fmt.Errorf("codex usage: missing access token or account_id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.token)
	req.Header.Set("Chatgpt-Account-Id", cred.accountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", codexCLIUserAgent)

	client := &http.Client{}
	if cred.proxyURL != "" {
		transport, _, errProxy := proxyutil.BuildHTTPTransport(cred.proxyURL)
		if errProxy != nil {
			return nil, fmt.Errorf("codex usage: proxy: %w", errProxy)
		}
		if transport != nil {
			defer transport.CloseIdleConnections()
			client.Transport = transport
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("quota-aware: close usage body: %v", errClose)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex usage: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, quotaUsageBodyLimit))
	if err != nil {
		return nil, err
	}
	return parseCodexUsage(body, now)
}

func parseCodexUsage(body []byte, now time.Time) (*codexUsageSnapshot, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("codex usage: payload is not an object")
	}
	rl := firstResult(root, "rate_limit", "rateLimit")
	snap := &codexUsageSnapshot{allowed: true}
	if v := rl.Get("allowed"); v.Exists() {
		snap.allowed = v.Bool()
	}
	if v := firstResult(rl, "limit_reached", "limitReached"); v.Exists() {
		snap.limitReached = v.Bool()
	}
	snap.primaryWindow = parseCodexWindow(firstResult(rl, "primary_window", "primaryWindow"), now)
	snap.secondaryWindow = parseCodexWindow(firstResult(rl, "secondary_window", "secondaryWindow"), now)
	return snap, nil
}

func parseCodexWindow(node gjson.Result, now time.Time) codexUsageWindow {
	var w codexUsageWindow
	if !node.IsObject() {
		return w
	}
	if used := firstResult(node, "used_percent", "usedPercent"); used.Exists() {
		if f, err := strconv.ParseFloat(strings.TrimSpace(used.String()), 64); err == nil {
			w.usedPercent = f
			w.hasUsedPercent = true
		}
	}
	if sec := firstResult(node, "reset_at", "resetAt").Int(); sec > 0 {
		w.resetAt = time.Unix(sec, 0)
	} else if sec := firstResult(node, "reset_after_seconds", "resetAfterSeconds").Int(); sec > 0 {
		w.resetAt = now.Add(time.Duration(sec) * time.Second)
	}
	return w
}

func firstResult(node gjson.Result, paths ...string) gjson.Result {
	for _, p := range paths {
		if v := node.Get(p); v.Exists() {
			return v
		}
	}
	return gjson.Result{}
}
