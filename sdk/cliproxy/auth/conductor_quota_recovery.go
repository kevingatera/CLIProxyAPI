package auth

import (
	"context"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// creditQuotaRecoveryTick is how often the sweep looks for cooled Codex credentials.
	creditQuotaRecoveryTick = 15 * time.Second
)

// creditQuotaRecovery probes Codex credentials that are cooling down for quota.
// The scheduler excludes those credentials before the quota selector sees them,
// so nothing else refreshes their usage, and a passive observation cannot end
// a credential_quota cooldown. This loop is the only path that can end one early.
type creditQuotaRecovery struct {
	now          func() time.Time
	fetch        func(context.Context, codexUsageCredential) (http.Header, error)
	hostProxyURL string

	mu      sync.Mutex
	entries map[string]*creditQuotaRecoveryEntry
}

type creditQuotaRecoveryEntry struct {
	// coolingSince is the first sweep that saw this cooldown. Only a probe started
	// after it can show capacity granted after the cooldown was set.
	coolingSince time.Time
	// recoverAt is the cooldown deadline this entry belongs to. A new deadline means a new cooldown.
	recoverAt   time.Time
	nextAttempt time.Time
	failures    int
	inflight    bool
}

// creditQuotaProbe is one scheduled usage read for a cooling credential.
type creditQuotaProbe struct {
	id           string
	cred         codexUsageCredential
	coolingSince time.Time
	recoverAt    time.Time
}

func newCreditQuotaRecovery(hostProxyURL string) *creditQuotaRecovery {
	return &creditQuotaRecovery{
		now:          time.Now,
		fetch:        fetchCodexUsage,
		hostProxyURL: hostProxyURL,
		entries:      make(map[string]*creditQuotaRecoveryEntry),
	}
}

// isCreditQuotaCooling reports whether a Codex credential is blocked by a quota
// cooldown that has not expired yet.
func isCreditQuotaCooling(a *Auth, now time.Time) bool {
	return isCodexQuotaAuth(a) && !a.Disabled && a.Quota.Exceeded &&
		a.Quota.Reason == "credential_quota" && a.Quota.NextRecoverAt.After(now)
}

// runCreditQuotaRecovery sweeps cooling credentials until ctx ends.
func (m *Manager) runCreditQuotaRecovery(ctx context.Context) {
	ticker := time.NewTicker(creditQuotaRecoveryTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, probe := range m.sweepCreditQuotaRecovery(ctx) {
				go m.runCreditQuotaProbe(ctx, probe)
			}
		}
	}
}

// sweepCreditQuotaRecovery returns the probes that are due now. It starts no I/O itself.
func (m *Manager) sweepCreditQuotaRecovery(ctx context.Context) []creditQuotaProbe {
	r := m.creditRecovery
	if m == nil || r == nil || ctx.Err() != nil {
		return nil
	}
	now := r.now()

	type cooling struct {
		cred      codexUsageCredential
		recoverAt time.Time
	}
	found := make(map[string]cooling)
	m.mu.RLock()
	for id, a := range m.auths {
		if isCreditQuotaCooling(a, now) {
			found[id] = cooling{cred: codexCredentialOf(a, r.hostProxyURL), recoverAt: a.Quota.NextRecoverAt}
		}
	}
	m.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	for id, entry := range r.entries {
		if _, ok := found[id]; !ok && !entry.inflight {
			delete(r.entries, id)
		}
	}
	var due []creditQuotaProbe
	for id, c := range found {
		entry := r.entries[id]
		if entry == nil || !entry.recoverAt.Equal(c.recoverAt) {
			if entry != nil && entry.inflight {
				// The old probe reports its own cooldown and cannot release this one.
				continue
			}
			entry = &creditQuotaRecoveryEntry{coolingSince: now, recoverAt: c.recoverAt}
			r.entries[id] = entry
		}
		if entry.inflight || now.Before(entry.nextAttempt) || !now.After(entry.coolingSince) {
			continue
		}
		entry.inflight = true
		due = append(due, creditQuotaProbe{id: id, cred: c.cred, coolingSince: entry.coolingSince, recoverAt: entry.recoverAt})
	}
	return due
}

// runCreditQuotaProbe reads usage for one cooling credential and ends its cooldown
// if the read proves capacity. Failures back off per credential.
func (m *Manager) runCreditQuotaProbe(ctx context.Context, probe creditQuotaProbe) {
	r := m.creditRecovery
	if r == nil {
		return
	}
	startedAt := r.now()
	headers, errFetch := r.fetch(ctx, probe.cred)
	released := false
	if errFetch == nil {
		released = m.applyCreditQuotaProbe(ctx, probe, startedAt, headers)
	}

	// The entry stays inflight until this section, so no sweep can start a second probe for it.
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[probe.id]
	if entry == nil || !entry.recoverAt.Equal(probe.recoverAt) {
		return
	}
	entry.inflight = false
	switch {
	case released:
		delete(r.entries, probe.id)
	case errFetch != nil:
		log.Debugf("credential quota recovery: probe failed for auth %s: %v", probe.id, errFetch)
		entry.failures++
		entry.nextAttempt = startedAt.Add(min(quotaRefreshBackoff<<min(entry.failures-1, 8), quotaRefreshBackoffMax))
	default:
		entry.failures = 0
		entry.nextAttempt = startedAt.Add(quotaRefreshInterval)
	}
}

// applyCreditQuotaProbe stores a probe as the credential's observation and ends
// the cooldown when the observation shows capacity. It reports whether it did.
func (m *Manager) applyCreditQuotaProbe(ctx context.Context, probe creditQuotaProbe, probedAt time.Time, headers http.Header) bool {
	r := m.creditRecovery
	now := r.now()

	releaseMutation := m.lockAuthMutation(probe.id)
	defer releaseMutation()
	m.mu.Lock()
	auth := m.auths[probe.id]
	if auth == nil || !isCreditQuotaCooling(auth, now) || !auth.Quota.NextRecoverAt.Equal(probe.recoverAt) {
		m.mu.Unlock()
		return false
	}
	if !probedAt.After(probe.coolingSince) {
		m.mu.Unlock()
		return false
	}
	if auth.Quota.ObservedAt.IsZero() || !auth.Quota.ObservedAt.After(probedAt) {
		auth.Quota.ObserveResponseHeadersForProvider("codex", headers, probedAt)
	}
	state, _, _ := classifyCodexSnapshot(codexSnapshotOf(auth.Quota, now), now)
	if state != quotaKnown {
		m.mu.Unlock()
		return false
	}

	cooldownStateChanged := false
	trackCooldownState := m.cooldownStore != nil
	var cooldownRecordsBefore []CooldownStateRecord
	if trackCooldownState {
		cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
	}
	if !clearCreditQuotaCooldownForAuth(auth, now) {
		m.mu.Unlock()
		return false
	}
	if trackCooldownState {
		cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, m.cooldownStateRecordsForAuthLocked(auth, now))
	}
	snapshot := auth.Clone()
	errPersist := m.persistLocked(ctx, auth)
	m.mu.Unlock()
	releaseMutation()

	if cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}
	m.publishAuthSchedulingState(probe.id, snapshot, now)
	if errPersist != nil {
		log.Warnf("credential quota recovery: persist auth %s: %v", probe.id, errPersist)
	}
	log.Infof("credential quota recovery: usage probe shows capacity, ended credential_quota cooldown for auth %s", probe.id)
	return true
}

// clearCreditQuotaCooldownForAuth ends only credential_quota cooldowns, on the
// credential and on the model states it propagated to. Other cooldowns stay.
// It reports whether anything changed.
func clearCreditQuotaCooldownForAuth(auth *Auth, now time.Time) bool {
	if auth == nil || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	changed := false
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" {
		if auth.NextRetryAfter.IsZero() || !auth.NextRetryAfter.After(auth.Quota.NextRecoverAt) {
			auth.Unavailable = false
			auth.NextRetryAfter = time.Time{}
		}
		applyCooldownFields(&auth.Quota, QuotaState{})
		changed = true
	}
	for _, state := range auth.ModelStates {
		if state == nil || !(state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota") {
			continue
		}
		if state.NextRetryAfter.IsZero() || !state.NextRetryAfter.After(state.Quota.NextRecoverAt) {
			state.Unavailable = false
			state.NextRetryAfter = time.Time{}
		}
		applyCooldownFields(&state.Quota, QuotaState{})
		state.credentialQuotaShadow = nil
		state.credentialQuotaPropagated = false
		state.UpdatedAt = now
		changed = true
	}
	if !changed {
		return false
	}
	if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	if !auth.Disabled && auth.Status != StatusDisabled && !hasModelError(auth, now) {
		auth.LastError = nil
		auth.StatusMessage = ""
		auth.Status = StatusActive
	}
	auth.Generation++
	auth.UpdatedAt = now
	return true
}
