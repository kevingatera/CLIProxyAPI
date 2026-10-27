package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type creditsRoutingExecutor struct {
	creditsPolicyExecutor
	plainStatus     int
	plainErr        error
	beforeCall      func(context.Context, *Auth)
	prepare         func(context.Context, *Auth)
	prepareErrors   map[string]error
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
	if err != nil && !AntigravityCreditsRequested(ctx) {
		if e.plainErr != nil {
			return e.plainErr
		}
		if e.plainStatus != 0 {
			return &Error{HTTPStatus: e.plainStatus, Message: "upstream unavailable"}
		}
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
	m, _ := newCreditsPolicyTestManager(t, []string{"alpha", "beta"}, policy, 0)
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
	for _, id := range []string{"alpha", "beta"} {
		registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: model}, {ID: "claude-source"}})
	}
	return m, e, model
}

// exhaustUnifiedCredits marks every AI credit pool of an auth as empty.
func exhaustUnifiedCredits(m *Manager, authID string) {
	now := time.Now()
	m.ObserveUnifiedQuota(authID, pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{
		{Key: "monthlyCredits"}, {Key: "purchasedCredits"}, {Key: "freeCredits"},
	}}, now)
}

func disableUnifiedAuth(m *Manager, authID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auths[authID].Disabled = true
}

// creditsRequestHeaders runs one request and returns the response headers the client would see,
// plus the terminal error. Stream headers ride on the stream result, nonstream on the error.
func creditsRequestHeaders(t *testing.T, m *Manager, model string, stream bool) (http.Header, error) {
	t.Helper()
	req := cliproxyexecutor.Request{Model: model}
	if !stream {
		_, err := m.Execute(context.Background(), []string{"antigravity"}, req, cliproxyexecutor.Options{})
		return SafeResponseHeaders(err), err
	}
	result, err := m.ExecuteStream(context.Background(), []string{"antigravity"}, req, cliproxyexecutor.Options{Stream: true})
	if err != nil {
		return SafeResponseHeaders(err), err
	}
	return result.Headers, drainStream(t, result)
}

// The credits lane serves plain failures. Cooldown and an exhausted plan allowance are the states it
// exists to get past; disabled credentials and exhausted AI credits stay out of both lanes.
func TestUnifiedCreditsLaneServesPlainFailures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			for _, tc := range []struct {
				name  string
				setup func(*Manager)
				want  string
				// creditsAuth is the credential that serves the credits lane.
				creditsAuth string
			}{
				{"both eligible", nil, "[alpha:plain beta:plain alpha:credits]", "alpha"},
				{"disabled peer", func(m *Manager) { disableUnifiedAuth(m, "alpha") }, "[beta:plain beta:credits]", "beta"},
				{"credit exhausted peer", func(m *Manager) { exhaustUnifiedCredits(m, "alpha") }, "[beta:plain beta:credits]", "beta"},
				{"allowance exhausted at plan build", func(m *Manager) {
					disableUnifiedAuth(m, "alpha")
					now := time.Now()
					m.ObserveUnifiedQuota("beta", reportUnified(0, now.Add(time.Hour)), now)
				}, "[beta:credits]", "beta"},
			} {
				t.Run(fmt.Sprintf("stream=%t/status=%d/%s", stream, status, tc.name), func(t *testing.T) {
					m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
					e.plainStatus = status
					if tc.setup != nil {
						tc.setup(m)
					}
					if err := runCreditsRoutingRequest(t, m, model, stream); err != nil {
						t.Fatalf("request failed: %v; calls = %s", err, fmt.Sprint(e.snapshot()))
					}
					if got := fmt.Sprint(e.snapshot()); got != tc.want {
						t.Fatalf("calls = %s, want %s", got, tc.want)
					}
					traces := m.ListRoutingTraces(10, "", "", false)
					if len(traces) != 1 || traces[0].FinalStatus != "success" {
						t.Fatalf("traces = %+v, want one successful trace", traces)
					}
					attempts := traces[0].Attempts
					last := attempts[len(attempts)-1]
					if !last.Credits || !last.Success || last.AuthID != tc.creditsAuth {
						t.Fatalf("final attempt = %+v, want a successful %s credits attempt", last, tc.creditsAuth)
					}
					for _, attempt := range attempts[:len(attempts)-1] {
						if attempt.Credits {
							t.Fatalf("plain attempt marked as credits: %+v", attempt)
						}
					}
				})
			}
		}
	}
}

// A credential that fails the credits attempt does not end the sweep: the next candidate still runs.
func TestUnifiedCreditsSweepContinuesPastCredentialFailures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			setup func(*creditsRoutingExecutor)
			want  string
			// wantStatus is the HTTP status recorded for alpha's failed credits attempt.
			wantStatus int
		}{
			{"prepare rejects with 400", func(e *creditsRoutingExecutor) {
				e.prepareErrors = map[string]error{"alpha": &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid credential"}}
			}, "[alpha:plain beta:plain beta:credits]", http.StatusBadRequest},
			{"upstream rejects with 401", func(e *creditsRoutingExecutor) {
				e.creditsErrors = map[string]error{"alpha": &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid credential"}}
			}, "[alpha:plain beta:plain alpha:credits beta:credits]", http.StatusUnauthorized},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
				e.plainStatus = http.StatusTooManyRequests
				tc.setup(e)
				if err := runCreditsRoutingRequest(t, m, model, stream); err != nil {
					t.Fatalf("request failed: %v; calls = %s", err, fmt.Sprint(e.snapshot()))
				}
				if got := fmt.Sprint(e.snapshot()); got != tc.want {
					t.Fatalf("calls = %s, want %s", got, tc.want)
				}
				traces := m.ListRoutingTraces(10, "", "", false)
				if len(traces) != 1 || traces[0].FinalStatus != "success" {
					t.Fatalf("traces = %+v, want one successful trace", traces)
				}
				var failed, succeeded []RoutingTraceAttempt
				for _, attempt := range traces[0].Attempts {
					if !attempt.Credits {
						continue
					}
					if attempt.Success {
						succeeded = append(succeeded, attempt)
					} else {
						failed = append(failed, attempt)
					}
				}
				if len(failed) != 1 || failed[0].AuthID != "alpha" || failed[0].HTTPStatus != tc.wantStatus || failed[0].Success {
					t.Fatalf("failed credits attempts = %+v, want one alpha failure with status %d", failed, tc.wantStatus)
				}
				if len(succeeded) != 1 || succeeded[0].AuthID != "beta" {
					t.Fatalf("successful credits attempts = %+v, want one beta success", succeeded)
				}
			})
		}
	}
}

// When the credits sweep ends in failure, the caller still gets the plain round's error with its
// upstream headers, in both the nonstream error and the stream bootstrap result.
func TestUnifiedCreditsFailureKeepsPlainHeaders(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
			retryAfter := 42 * time.Second
			e.plainErr = issue7017HintError{msg: "quota exhausted", hint: &retryAfter}
			e.creditsErrors = map[string]error{
				"alpha": &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid credential"},
				"beta":  &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid credential"},
			}
			headers, err := creditsRequestHeaders(t, m, model, stream)
			if err == nil {
				t.Fatal("request succeeded with every credits credential rejected")
			}
			if got := fmt.Sprint(e.snapshot()); got != "[alpha:plain beta:plain alpha:credits beta:credits]" {
				t.Fatalf("calls = %s", got)
			}
			if got := headers.Get("Retry-After"); got != "42" {
				t.Fatalf("Retry-After = %q, want 42 from the plain failure (err=%v)", got, err)
			}
			traces := m.ListRoutingTraces(10, "", "", false)
			if len(traces) != 1 || traces[0].FinalStatus != "failed" || len(traces[0].Attempts) != 4 {
				t.Fatalf("traces = %+v, want one failed trace with four attempts", traces)
			}
		})
	}
}

// A request-level termination ends the sweep for every candidate rather than just one credential.
func TestUnifiedCreditsSweepStopsOnRequestTermination(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
			e.plainStatus = http.StatusTooManyRequests
			e.creditsErrors = map[string]error{"alpha": &cliproxyexecutor.RequestTerminatedError{HTTPStatus: http.StatusForbidden, Body: []byte("stopped")}}
			err := runCreditsRoutingRequest(t, m, model, stream)
			var terminated *cliproxyexecutor.RequestTerminatedError
			if !errors.As(err, &terminated) {
				t.Fatalf("error = %v, want the request termination", err)
			}
			if got := fmt.Sprint(e.snapshot()); got != "[alpha:plain beta:plain alpha:credits]" {
				t.Fatalf("calls = %s, want the sweep to stop after alpha", got)
			}
			traces := m.ListRoutingTraces(10, "", "", false)
			if len(traces) != 1 || traces[0].FinalStatus != "failed" {
				t.Fatalf("traces = %+v, want one failed trace", traces)
			}
		})
	}
}

// A credential that becomes credits-exhausted between planning and dispatch must not reach the credits
// lane. The recheck after credential preparation is what keeps it out.
func TestUnifiedCreditsRechecksEligibilityAfterConcurrentPreparation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			m, e, model := newUnifiedCreditsRoutingManager(t, antigravityPinnedPolicy("beta"))
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
					exhaustUnifiedCredits(m, "beta")
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
				t.Fatal("credits dispatch ignored the updated credit balance")
			}
			if got, want := fmt.Sprint(e.snapshot()), "[beta:plain]"; got != want {
				t.Fatalf("calls = %s, want %s", got, want)
			}
		})
	}
}

func TestCreditsFallbackRespectsFailurePolicy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, unified := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/unified=%t", stream, unified), func(t *testing.T) {
				policy := antigravityPinnedPolicy("beta")
				policy.Fallback.On = []string{internalconfig.RoutingFallbackTransportError}
				var m *Manager
				var e *creditsRoutingExecutor
				model := "claude-credits-policy"
				if unified {
					m, e, model = newUnifiedCreditsRoutingManager(t, policy)
				} else {
					m, _ = newCreditsPolicyTestManager(t, []string{"alpha", "beta"}, policy, 0)
					e = &creditsRoutingExecutor{}
					m.RegisterExecutor(e)
				}
				if err := runCreditsRoutingRequest(t, m, model, stream); err == nil {
					t.Fatal("request succeeded despite terminal failure policy")
				}
				if got, want := fmt.Sprint(e.snapshot()), "[beta:plain]"; got != want {
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
					policy := antigravityPinnedPolicy("beta")
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
						m, _ = newCreditsPolicyTestManager(t, []string{"alpha", "beta"}, policy, 0)
						e = &creditsRoutingExecutor{}
						m.RegisterExecutor(e)
					}
					e.failCredits = fail
					err := runCreditsRoutingRequest(t, m, model, stream)
					if (err != nil) != fail {
						t.Fatalf("request error = %v, want failure %t", err, fail)
					}
					if got, want := fmt.Sprint(e.snapshot()), "[beta:plain beta:credits]"; got != want {
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
						if attempt.AuthID != "beta" || fmt.Sprint(attempt.Providers) != "[antigravity]" {
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

func (e *creditsRoutingExecutor) ShouldPrepareRequestAuth(*Auth) bool {
	return e.prepare != nil || len(e.prepareErrors) > 0
}

func (e *creditsRoutingExecutor) PrepareRequestAuth(ctx context.Context, a *Auth) (*Auth, error) {
	if e.prepare != nil {
		e.prepare(ctx, a)
	}
	if AntigravityCreditsRequested(ctx) {
		if errPrepare, ok := e.prepareErrors[a.ID]; ok {
			return nil, errPrepare
		}
	}
	return a, nil
}

// Credentials with exhausted AI credits are dropped before any hint lookup, so a hint-store outage
// cannot fail the sweep on their account.
func TestUnifiedCreditsCandidatesSkipIneligibleBeforeHintLookup(t *testing.T) {
	m, _, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
	exhaustUnifiedCredits(m, "alpha")
	exhaustUnifiedCredits(m, "beta")
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
