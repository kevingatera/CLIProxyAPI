package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type creditsRoutingExecutor struct {
	creditsPolicyExecutor
	plainStatus     int
	beforeCall      func(context.Context, *Auth)
	prepare         func(context.Context, *Auth)
	creditsErrors   map[string]error
	streamTailError error
}

func (e *creditsRoutingExecutor) execute(ctx context.Context, a *Auth) error {
	if e.beforeCall != nil {
		e.beforeCall(ctx, a)
	}
	err := e.record(ctx, a)
	if AntigravityCreditsRequested(ctx) {
		if errCredits, ok := e.creditsErrors[a.ID]; ok {
			return errCredits
		}
	}
	if err != nil && !AntigravityCreditsRequested(ctx) && e.plainStatus != 0 {
		return &Error{HTTPStatus: e.plainStatus, Message: "upstream unavailable"}
	}
	return err
}

func (e *creditsRoutingExecutor) Execute(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte("completion")}, e.execute(ctx, a)
}

func (e *creditsRoutingExecutor) ExecuteStream(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.execute(ctx, a); err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("chunk")}
	if e.streamTailError != nil {
		chunks <- cliproxyexecutor.StreamChunk{Err: e.streamTailError}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func runCreditsRoutingRequest(t *testing.T, m *Manager, model string, stream bool) error {
	t.Helper()
	req := cliproxyexecutor.Request{Model: model}
	if !stream {
		_, err := m.Execute(context.Background(), []string{"antigravity"}, req, cliproxyexecutor.Options{})
		return err
	}
	result, err := m.ExecuteStream(context.Background(), []string{"antigravity"}, req, cliproxyexecutor.Options{Stream: true})
	if err == nil {
		err = drainStream(t, result)
	}
	return err
}

func newUnifiedCreditsRoutingManager(t *testing.T, policy internalconfig.RoutingPolicy) (*Manager, *creditsRoutingExecutor, string) {
	t.Helper()
	const model = "cliproxy/claude-credits-review"
	m, _ := newCreditsPolicyTestManager(t, []string{"a-excluded", "b-eligible"}, policy, 0)
	e := &creditsRoutingExecutor{}
	m.RegisterExecutor(e)
	for _, a := range m.List() {
		a.Metadata["disable_cooling"] = false
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	m.SetConfig(&internalconfig.Config{
		QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		Routing:       internalconfig.RoutingConfig{Policy: policy, UnifiedModels: internalconfig.UnifiedModels{Enabled: true, Models: []internalconfig.UnifiedModel{{ID: "claude-credits-review", Routes: []internalconfig.UnifiedModelRoute{{Provider: "antigravity", Source: "claude-source", Model: "claude-wire", Plan: "included"}}}}}},
	})
	for _, id := range []string{"a-excluded", "b-eligible"} {
		registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: model}, {ID: "claude-source"}})
	}
	return m, e, model
}

func TestUnifiedCreditsFallbackRejectsIneligibleCredentials(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			for _, exclusion := range []string{"quota", "cooldown", "new-quota", "new-cooldown"} {
				t.Run(fmt.Sprintf("stream=%t/status=%d/%s", stream, status, exclusion), func(t *testing.T) {
					m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{})
					e.plainStatus = status
					exclude := func() {
						if exclusion == "quota" || exclusion == "new-quota" {
							now := time.Now()
							m.ObserveUnifiedQuota("a-excluded", reportUnified(0, now.Add(time.Hour)), now)
						} else {
							m.mu.Lock()
							m.auths["a-excluded"].ModelStates = map[string]*ModelState{"claude-wire": {Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}}
							m.mu.Unlock()
						}
					}
					if exclusion == "quota" || exclusion == "cooldown" {
						exclude()
					} else {
						// Both credentials are initially eligible; b runs first and changes a's eligibility.
						m.ObserveUnifiedQuota("b-eligible", reportUnified(1, time.Now().Add(time.Hour)), time.Now())
						e.beforeCall = func(ctx context.Context, a *Auth) {
							if !AntigravityCreditsRequested(ctx) && a.ID == "b-eligible" {
								exclude()
							}
						}
					}
					if err := runCreditsRoutingRequest(t, m, model, stream); err == nil {
						t.Fatal("request succeeded through an ineligible credits credential")
					}
					if got, want := fmt.Sprint(e.snapshot()), "[b-eligible:plain]"; got != want {
						t.Fatalf("calls = %s, want %s", got, want)
					}
				})
			}
		}
	}
}

func TestUnifiedCreditsCandidatesSkipIneligibleBeforeHintLookup(t *testing.T) {
	m, _, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{})
	now := time.Now()
	for _, id := range []string{"a-excluded", "b-eligible"} {
		m.ObserveUnifiedQuota(id, reportUnified(0, now.Add(time.Hour)), now)
	}
	// Hint lookups fail in this mode, so any read for an excluded credential surfaces as an error.
	homekv.SetCurrent(homekv.New(internalconfig.HomeConfig{Enabled: false}))
	t.Cleanup(homekv.ClearCurrent)
	candidates, err := m.findAllAntigravityCreditsCandidateAuths(context.Background(), model, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("findAllAntigravityCreditsCandidateAuths() error = %v, want excluded credentials skipped without a hint lookup", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %d, want 0", len(candidates))
	}
}

func TestCreditsFallbackRespectsFailurePolicy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, unified := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/unified=%t", stream, unified), func(t *testing.T) {
				policy := antigravityPinnedPolicy("b-eligible")
				policy.Fallback.On = []string{internalconfig.RoutingFallbackTransportError}
				var m *Manager
				var e *creditsRoutingExecutor
				model := "claude-credits-policy"
				if unified {
					m, e, model = newUnifiedCreditsRoutingManager(t, policy)
				} else {
					m, _ = newCreditsPolicyTestManager(t, []string{"a-excluded", "b-eligible"}, policy, 0)
					e = &creditsRoutingExecutor{}
					m.RegisterExecutor(e)
				}
				if err := runCreditsRoutingRequest(t, m, model, stream); err == nil {
					t.Fatal("request succeeded despite terminal failure policy")
				}
				if got, want := fmt.Sprint(e.snapshot()), "[b-eligible:plain]"; got != want {
					t.Fatalf("calls = %s, want %s", got, want)
				}
			})
		}
	}
}

func TestCreditsFallbackRecordsRoutingAttempts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, unified := range []bool{false, true} {
			for _, fail := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/unified=%t/fail=%t", stream, unified, fail), func(t *testing.T) {
					policy := antigravityPinnedPolicy("b-eligible")
					var m *Manager
					var e *creditsRoutingExecutor
					model := "claude-credits-policy"
					if unified {
						m, e, model = newUnifiedCreditsRoutingManager(t, policy)
						for _, a := range m.List() {
							a.Metadata["disable_cooling"] = true
							if _, err := m.Update(context.Background(), a); err != nil {
								t.Fatal(err)
							}
						}
					} else {
						m, _ = newCreditsPolicyTestManager(t, []string{"a-excluded", "b-eligible"}, policy, 0)
						e = &creditsRoutingExecutor{}
						m.RegisterExecutor(e)
					}
					e.failCredits = fail
					err := runCreditsRoutingRequest(t, m, model, stream)
					if (err != nil) != fail {
						t.Fatalf("request error = %v, want failure %t", err, fail)
					}
					if got, want := fmt.Sprint(e.snapshot()), "[b-eligible:plain b-eligible:credits]"; got != want {
						t.Fatalf("calls = %s, want %s", got, want)
					}
					traces := m.ListRoutingTraces(10, model, "antigravity", false)
					if len(traces) != 1 {
						t.Fatalf("traces = %+v, want one trace", traces)
					}
					trace := traces[0]
					if len(trace.Attempts) != 2 {
						t.Fatalf("attempts = %+v, want plain and credits attempts", trace.Attempts)
					}
					for i, attempt := range trace.Attempts {
						if attempt.AuthID != "b-eligible" || fmt.Sprint(attempt.Providers) != "[antigravity]" {
							t.Fatalf("attempt %d = %+v", i, attempt)
						}
					}
					credits := trace.Attempts[1]
					if !credits.Credits || trace.Attempts[0].Credits || credits.Success == fail {
						t.Fatalf("credits attempt = %+v, want success %t", credits, !fail)
					}
					wantStatus := "success"
					if fail {
						wantStatus = "failed"
						if credits.HTTPStatus != http.StatusTooManyRequests || credits.Error == "" || credits.FallbackReason != internalconfig.RoutingFallbackRateLimited {
							t.Fatalf("missing credits failure details: %+v", credits)
						}
					}
					if trace.FinalStatus != wantStatus {
						t.Fatalf("trace = %+v, want final status %s", trace, wantStatus)
					}
				})
			}
		}
	}
}

func (e *creditsRoutingExecutor) ShouldPrepareRequestAuth(*Auth) bool { return e.prepare != nil }

func (e *creditsRoutingExecutor) PrepareRequestAuth(ctx context.Context, a *Auth) (*Auth, error) {
	e.prepare(ctx, a)
	return a, nil
}

func TestUnifiedCreditsRechecksQuotaAfterConcurrentPreparation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			m, e, model := newUnifiedCreditsRoutingManager(t, antigravityPinnedPolicy("b-eligible"))
			for _, a := range m.List() {
				a.Metadata["disable_cooling"] = true
				if _, err := m.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			preparing, updated := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go func() {
				defer close(updated)
				select {
				case <-preparing:
					now := time.Now()
					m.ObserveUnifiedQuota("b-eligible", reportUnified(0, now.Add(time.Hour)), now)
				case <-ctx.Done():
				}
			}()
			e.prepare = func(ctx context.Context, a *Auth) {
				if AntigravityCreditsRequested(ctx) {
					close(preparing)
					select {
					case <-updated:
					case <-time.After(5 * time.Second):
						t.Fatal("concurrent quota update blocked")
					}
				}
			}
			if err := runCreditsRoutingRequest(t, m, model, stream); err == nil {
				t.Fatal("credits dispatch ignored the updated quota")
			}
			if got, want := fmt.Sprint(e.snapshot()), "[b-eligible:plain]"; got != want {
				t.Fatalf("calls = %s, want %s", got, want)
			}
		})
	}
}

func TestCreditsFallbackTracesEveryCandidate(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			policy := antigravityPinnedPolicy("b")
			policy.Defaults.Route[0].AuthOrder = []string{"b", "a"}
			m, _ := newCreditsPolicyTestManager(t, []string{"a", "b"}, policy, 0)
			e := &creditsRoutingExecutor{creditsErrors: map[string]error{"a": &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "credits unavailable"}}}
			m.RegisterExecutor(e)
			if err := runCreditsRoutingRequest(t, m, "claude-credits-policy", stream); err != nil {
				t.Fatal(err)
			}
			if got, want := fmt.Sprint(e.snapshot()), "[b:plain a:plain a:credits b:credits]"; got != want {
				t.Fatalf("calls = %s, want %s", got, want)
			}
			traces := m.ListRoutingTraces(10, "", "", false)
			if len(traces) != 1 || len(traces[0].Attempts) != 4 {
				t.Fatalf("traces = %+v", traces)
			}
			attempts := traces[0].Attempts
			if !attempts[2].Credits || attempts[2].Success || attempts[2].AuthID != "a" || attempts[2].HTTPStatus != http.StatusServiceUnavailable || !attempts[3].Credits || !attempts[3].Success || attempts[3].AuthID != "b" {
				t.Fatalf("credits attempts = %+v", attempts[2:])
			}
		})
	}
}

func TestCreditsFallbackStreamCompletionTrace(t *testing.T) {
	m, _ := newCreditsPolicyTestManager(t, []string{"b"}, antigravityPinnedPolicy("b"), 0)
	e := &creditsRoutingExecutor{streamTailError: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "stream failed"}}
	m.RegisterExecutor(e)
	if err := runCreditsRoutingRequest(t, m, "claude-credits-policy", true); err == nil {
		t.Fatal("want stream completion error")
	}
	traces := m.ListRoutingTraces(10, "", "", false)
	if len(traces) != 1 || traces[0].FinalStatus != "failed" || len(traces[0].Attempts) != 2 || !traces[0].Attempts[1].Credits || !traces[0].Attempts[1].Success || traces[0].Error == "" {
		t.Fatalf("traces = %+v", traces)
	}
}

func TestCreditsFallbackConcurrentRequestTraces(t *testing.T) {
	m, _ := newCreditsPolicyTestManager(t, []string{"b"}, antigravityPinnedPolicy("b"), 0)
	const requests = 12
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(stream bool) {
			defer wg.Done()
			if err := runCreditsRoutingRequest(t, m, "claude-credits-policy", stream); err != nil {
				t.Errorf("request failed: %v", err)
			}
		}(i%2 == 0)
	}
	wg.Wait()
	traces := m.ListRoutingTraces(requests, "", "", false)
	if len(traces) != requests {
		t.Fatalf("traces = %d, want %d", len(traces), requests)
	}
	seen := make(map[string]bool)
	for _, trace := range traces {
		if seen[trace.ID] || trace.FinalStatus != "success" || len(trace.Attempts) != 2 || !trace.Attempts[1].Credits || !trace.Attempts[1].Success {
			t.Fatalf("trace = %+v", trace)
		}
		seen[trace.ID] = true
	}
}
