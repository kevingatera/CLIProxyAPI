package auth

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const maxRoutingTraceErrorLength = 300

// RoutingTrace records how one request walked the routing policy.
type RoutingTrace struct {
	ID          string                `json:"id"`
	Timestamp   time.Time             `json:"timestamp"`
	Operation   string                `json:"operation"`
	Model       string                `json:"model"`
	Providers   []string              `json:"providers"`
	FallbackOn  []string              `json:"fallback_on,omitempty"`
	Attempts    []RoutingTraceAttempt `json:"attempts,omitempty"`
	FinalStatus string                `json:"final_status"`
	Error       string                `json:"error,omitempty"`
}

// RoutingTraceAttempt records one route step.
type RoutingTraceAttempt struct {
	Providers      []string `json:"providers"`
	AuthID         string   `json:"auth_id,omitempty"`
	Credits        bool     `json:"credits,omitempty"`
	Success        bool     `json:"success"`
	HTTPStatus     int      `json:"http_status,omitempty"`
	Error          string   `json:"error,omitempty"`
	Fallback       bool     `json:"fallback,omitempty"`
	FallbackReason string   `json:"fallback_reason,omitempty"`
}

// routingTraceRing is a fixed-capacity ring buffer. Inserts overwrite the oldest entry in place.
type routingTraceRing struct {
	mu   sync.Mutex
	buf  []RoutingTrace
	next int
	size int
}

func (r *routingTraceRing) add(trace RoutingTrace, capacity int) {
	if capacity <= 0 {
		capacity = defaultRoutingTraceLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) != capacity {
		r.resizeLocked(capacity)
	}
	r.buf[r.next] = trace
	r.next = (r.next + 1) % capacity
	if r.size < capacity {
		r.size++
	}
}

// resizeLocked keeps the newest entries when the configured capacity changes.
func (r *routingTraceRing) resizeLocked(capacity int) {
	ordered := r.orderedLocked()
	if len(ordered) > capacity {
		ordered = ordered[len(ordered)-capacity:]
	}
	r.buf = make([]RoutingTrace, capacity)
	copy(r.buf, ordered)
	r.size = len(ordered)
	r.next = r.size % capacity
}

// orderedLocked returns entries oldest first.
func (r *routingTraceRing) orderedLocked() []RoutingTrace {
	out := make([]RoutingTrace, 0, r.size)
	start := (r.next - r.size + len(r.buf)) % max(len(r.buf), 1)
	for i := 0; i < r.size; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

// ListRoutingTraces returns recent routing traces, newest first.
func (m *Manager) ListRoutingTraces(limit int, model, provider string, failedOnly bool) []RoutingTrace {
	if m == nil {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	model = strings.TrimSpace(model)
	provider = strings.TrimSpace(provider)

	m.routingTraces.mu.Lock()
	ordered := m.routingTraces.orderedLocked()
	m.routingTraces.mu.Unlock()

	result := make([]RoutingTrace, 0, min(limit, len(ordered)))
	for i := len(ordered) - 1; i >= 0 && len(result) < limit; i-- {
		trace := ordered[i]
		if failedOnly && trace.FinalStatus == "success" {
			continue
		}
		if model != "" && !strings.EqualFold(trace.Model, model) {
			continue
		}
		if provider != "" && !containsFold(trace.Providers, provider) {
			continue
		}
		result = append(result, trace)
	}
	return result
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func sortedTriggerList(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for trigger := range set {
		out = append(out, trigger)
	}
	sort.Strings(out)
	return out
}

func truncateRoutingTraceError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if len(text) > maxRoutingTraceErrorLength {
		text = text[:maxRoutingTraceErrorLength] + "..."
	}
	return text
}

func (p *routingPlan) recordAttempt(step routingStep, err error, reason string, fallback bool) {
	if p == nil {
		return
	}
	p.trace.Attempts = append(p.trace.Attempts, RoutingTraceAttempt{
		Providers:      append([]string(nil), step.providers...),
		AuthID:         step.authID,
		Success:        err == nil,
		HTTPStatus:     statusCodeFromError(err),
		Error:          truncateRoutingTraceError(err),
		Fallback:       fallback,
		FallbackReason: reason,
	})
}

// finish stores the trace. It runs once per request, after the final outcome is known.
func (p *routingPlan) finish(err error) {
	if p == nil || p.manager == nil {
		return
	}
	p.trace.FinalStatus = "success"
	if err != nil {
		p.trace.FinalStatus = "failed"
		p.trace.Error = truncateRoutingTraceError(err)
	}
	p.manager.routingTraces.add(p.trace, p.traceLimit)
}

// finishStream defers the trace until the stream ends, so a stream that fails after it
// starts is recorded as failed rather than as a success at bootstrap.
func (p *routingPlan) finishStream(ctx context.Context, result *cliproxyexecutor.StreamResult, err error) *cliproxyexecutor.StreamResult {
	if p == nil {
		return result
	}
	if err != nil || result == nil || result.Chunks == nil {
		p.finish(err)
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		var streamErr error
		defer func() {
			p.finish(streamErr)
			close(out)
		}()
		for {
			select {
			case <-ctx.Done():
				streamErr = ctx.Err()
				return
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				if chunk.Err != nil && streamErr == nil {
					streamErr = chunk.Err
				}
				select {
				case <-ctx.Done():
					streamErr = ctx.Err()
					return
				case out <- chunk:
				}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}
}

// Credits attempts run after the plain routing round but belong to the same request trace.
func (p *routingPlan) recordCreditsAttempt(ctx context.Context, auth *Auth, err error) {
	if p == nil {
		return
	}
	reason := classifyRoutingFailure(ctx, err)
	p.recordAttempt(routingStep{providers: []string{auth.Provider}, authID: auth.ID}, err, reason, p.allowsFallback(reason))
	p.trace.Attempts[len(p.trace.Attempts)-1].Credits = true
}
