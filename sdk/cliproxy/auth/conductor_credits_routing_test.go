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
	// creditsBootstrap makes a credits stream fail at bootstrap, with creditsHeaders on the stream result.
	creditsBootstrap map[string]error
	creditsHeaders   http.Header
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
	if errBootstrap, ok := e.creditsBootstrap[a.ID]; ok && AntigravityCreditsRequested(ctx) {
		e.record(ctx, a)
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		chunks <- cliproxyexecutor.StreamChunk{Err: errBootstrap}
		close(chunks)
		return &cliproxyexecutor.StreamResult{Headers: e.creditsHeaders, Chunks: chunks}, nil
	}
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

// creditsStatusError is an upstream rejection with a status and a Retry-After hint.
type creditsStatusError struct {
	status     int
	msg        string
	retryAfter time.Duration
}

func (e creditsStatusError) Error() string              { return e.msg }
func (e creditsStatusError) StatusCode() int            { return e.status }
func (e creditsStatusError) RetryAfter() *time.Duration { return &e.retryAfter }

// Plain round never reaches upstream (no candidate), so the credits attempt's own failure is the answer.
func TestUnifiedCreditsFailureReplacesSyntheticPlainError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			t.Run(fmt.Sprintf("stream=%t/status=%d", stream, status), func(t *testing.T) {
				m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
				disableUnifiedAuth(m, "alpha")
				now := time.Now()
				m.ObserveUnifiedQuota("beta", reportUnified(0, now.Add(time.Hour)), now)
				e.creditsErrors = map[string]error{"beta": creditsStatusError{status: status, msg: "credits rejected", retryAfter: 37 * time.Second}}
				headers, err := creditsRequestHeaders(t, m, model, stream)
				if err == nil {
					t.Fatal("request succeeded with the only credential rejected")
				}
				if got := statusCodeFromError(err); got != status {
					t.Fatalf("status = %d, want the credits failure status %d (err=%v)", got, status, err)
				}
				if got := headers.Get("Retry-After"); got != "37" {
					t.Fatalf("Retry-After = %q, want 37 from the credits failure (err=%v)", got, err)
				}
				if got := fmt.Sprint(e.snapshot()); got != "[beta:credits]" {
					t.Fatalf("calls = %s, want [beta:credits]", got)
				}
				traces := m.ListRoutingTraces(10, "", "antigravity", false)
				if len(traces) != 1 || traces[0].FinalStatus != "failed" || len(traces[0].Attempts) != 1 {
					t.Fatalf("traces = %+v, want one failed credits-only trace", traces)
				}
				if attempt := traces[0].Attempts[0]; !attempt.Credits || attempt.Success || attempt.HTTPStatus != status {
					t.Fatalf("attempt = %+v, want a failed credits attempt with status %d", attempt, status)
				}
			})
		}
	}
}

// Credits-only requests have no plain provider, so the trace names the credits provider for filtering.
func TestUnifiedCreditsOnlyTraceNamesProvider(t *testing.T) {
	m, _, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
	disableUnifiedAuth(m, "alpha")
	now := time.Now()
	m.ObserveUnifiedQuota("beta", reportUnified(0, now.Add(time.Hour)), now)
	if err := runCreditsRoutingRequest(t, m, model, false); err != nil {
		t.Fatal(err)
	}
	traces := m.ListRoutingTraces(10, "", "antigravity", false)
	if len(traces) != 1 {
		t.Fatalf("provider-filtered traces = %+v, want the credits-only trace", traces)
	}
	if got := fmt.Sprint(traces[0].Providers); got != "[antigravity]" {
		t.Fatalf("trace providers = %s, want [antigravity]", got)
	}
}

// A credits stream bootstrap error keeps the headers the upstream sent, both when it is the final error and
// when a terminal credits error ends the sweep.
func TestUnifiedCreditsStreamBootstrapKeepsHeaders(t *testing.T) {
	headers := http.Header{"Retry-After": []string{"37"}}
	t.Run("final credits bootstrap failure", func(t *testing.T) {
		m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
		disableUnifiedAuth(m, "alpha")
		now := time.Now()
		m.ObserveUnifiedQuota("beta", reportUnified(0, now.Add(time.Hour)), now)
		e.creditsBootstrap = map[string]error{"beta": creditsStatusError{status: http.StatusTooManyRequests, msg: "credits rejected", retryAfter: 37 * time.Second}}
		e.creditsHeaders = headers
		got, err := creditsRequestHeaders(t, m, model, true)
		if err == nil {
			t.Fatal("stream succeeded with the only credential rejected at bootstrap")
		}
		if got.Get("Retry-After") != "37" {
			t.Fatalf("stream Retry-After = %q, want 37 from the credits bootstrap (err=%v)", got.Get("Retry-After"), err)
		}
		if statusCodeFromError(err) != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (err=%v)", statusCodeFromError(err), err)
		}
	})
	t.Run("terminal credits bootstrap failure", func(t *testing.T) {
		m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
		e.plainStatus = http.StatusTooManyRequests
		e.creditsBootstrap = map[string]error{"alpha": &cliproxyexecutor.RequestTerminatedError{HTTPStatus: http.StatusForbidden, Body: []byte("stopped")}}
		e.creditsHeaders = headers
		got, err := creditsRequestHeaders(t, m, model, true)
		var terminated *cliproxyexecutor.RequestTerminatedError
		if !errors.As(err, &terminated) {
			t.Fatalf("error = %v, want the request termination", err)
		}
		if got.Get("Retry-After") != "37" {
			t.Fatalf("stream Retry-After = %q, want 37 from the terminated bootstrap", got.Get("Retry-After"))
		}
		if calls := fmt.Sprint(e.snapshot()); calls != "[alpha:plain beta:plain alpha:credits]" {
			t.Fatalf("calls = %s, want the sweep to stop after alpha", calls)
		}
	})
}

// A credential in the terminal unauthorized state (a refresh failed with 401 or invalid_grant) gets no credits
// attempts, while cooldown from a plain 429 on the other credential still does not exclude it from credits.
func TestUnifiedCreditsSkipsTerminallyRejectedCredential(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
			e.plainStatus = http.StatusTooManyRequests
			markUnifiedAuthTerminal(m, "alpha")
			if err := runCreditsRoutingRequest(t, m, model, stream); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(e.snapshot()); got != "[beta:plain beta:credits]" {
				t.Fatalf("calls = %s, want [beta:plain beta:credits]", got)
			}
		})
	}
}

// Requests race the moment a credential becomes terminally rejected. Once the wave settles, no later request
// may reach that credential's credits lane, and the untouched credential keeps serving.
func TestUnifiedCreditsConcurrentTerminalRejection(t *testing.T) {
	m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
	e.plainStatus = http.StatusTooManyRequests
	e.creditsErrors = map[string]error{"alpha": &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid credential"}}
	const requests = 8
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(stream bool) {
			defer wg.Done()
			errs <- runCreditsRoutingRequest(t, m, model, stream)
		}(i%2 == 0)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		markUnifiedAuthTerminal(m, "alpha")
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent request failed: %v", err)
		}
	}
	// A credits 401 can re-mark alpha after the terminal write, so the final state is set once the wave has settled.
	markUnifiedAuthTerminal(m, "alpha")
	before := len(e.snapshot())
	if err := runCreditsRoutingRequest(t, m, model, false); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(e.snapshot()[before:]); got != "[beta:plain beta:credits]" {
		t.Fatalf("post-wave calls = %s, want [beta:plain beta:credits]", got)
	}
}

// An antigravity credential that the unified model does not route to never enters the credits lane, even
// when the request's plan filter is absent.
func TestUnifiedCreditsExcludesUnroutedCredential(t *testing.T) {
	m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
	registerRetryRoundLocalAuths(t, m, "antigravity", "claude-other-source", map[string]int{"gamma": 0})
	candidates, err := m.findAllAntigravityCreditsCandidateAuths(context.Background(), model, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.auth.ID == "gamma" {
			t.Fatalf("unrouted credential gamma offered to credits: %+v", candidates)
		}
	}
	disableUnifiedAuth(m, "alpha")
	now := time.Now()
	m.ObserveUnifiedQuota("beta", reportUnified(0, now.Add(time.Hour)), now)
	if err := runCreditsRoutingRequest(t, m, model, false); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(e.snapshot()); got != "[beta:credits]" {
		t.Fatalf("calls = %s, want [beta:credits]", got)
	}
}

// markUnifiedAuthTerminal puts an auth in the terminal unauthorized state: a refresh failed with 401 after a
// credits rejection, so no refresh is pending and the credential is not retried.
func markUnifiedAuthTerminal(m *Manager, authID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[authID]
	auth.Unavailable = true
	auth.Status = StatusError
	auth.LastError = &Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "invalid credential"}
	auth.NextRetryAfter = time.Time{}
	auth.NextRefreshAfter = time.Time{}
}

// unifiedAuthTerminallyRejected reports whether the auth carries the terminal unauthorized state.
func unifiedAuthTerminallyRejected(t *testing.T, m *Manager, authID string) bool {
	t.Helper()
	auth, ok := m.GetByID(authID)
	if !ok {
		t.Fatalf("auth %s missing", authID)
	}
	return hasUnauthorizedAuthFailure(auth)
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

const creditsInvalidRequestBody = `{"error":{"type":"invalid_request_error","message":"bad request"}}`

// A credits attempt that the upstream rejects as a request fault is terminal, as on the plain pipeline. The
// sweep must not try another credential, so the client does not get a later credential's 503 and Retry-After.
func TestUnifiedCreditsSweepStopsOnRequestInvalid(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
			e.plainStatus = http.StatusTooManyRequests
			e.creditsErrors = map[string]error{
				"alpha": &Error{HTTPStatus: http.StatusBadRequest, Message: creditsInvalidRequestBody},
				"beta":  creditsStatusError{status: http.StatusServiceUnavailable, msg: "busy", retryAfter: 37 * time.Second},
			}
			headers, err := creditsRequestHeaders(t, m, model, stream)
			if err == nil {
				t.Fatal("request succeeded with a request-invalid credits rejection")
			}
			if got := statusCodeFromError(err); got != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 from the request fault (err=%v)", got, err)
			}
			if got := headers.Get("Retry-After"); got != "" {
				t.Fatalf("Retry-After = %q, want none from the request fault (err=%v)", got, err)
			}
			if got := fmt.Sprint(e.snapshot()); got != "[alpha:plain beta:plain alpha:credits]" {
				t.Fatalf("calls = %s, want the sweep to stop after alpha", got)
			}
		})
	}
}

// A request-invalid credits bootstrap keeps the upstream headers on the terminal stream result.
func TestUnifiedCreditsStreamInvalidBootstrapKeepsHeaders(t *testing.T) {
	m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
	e.plainStatus = http.StatusTooManyRequests
	e.creditsBootstrap = map[string]error{"alpha": &Error{HTTPStatus: http.StatusBadRequest, Message: creditsInvalidRequestBody}}
	e.creditsHeaders = http.Header{"Retry-After": []string{"37"}}
	got, err := creditsRequestHeaders(t, m, model, true)
	if err == nil {
		t.Fatal("stream succeeded with a request-invalid credits bootstrap")
	}
	if statusCodeFromError(err) != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (err=%v)", statusCodeFromError(err), err)
	}
	if got.Get("Retry-After") != "37" {
		t.Fatalf("stream Retry-After = %q, want 37 from the credits bootstrap (err=%v)", got.Get("Retry-After"), err)
	}
	if calls := fmt.Sprint(e.snapshot()); calls != "[alpha:plain beta:plain alpha:credits]" {
		t.Fatalf("calls = %s, want the sweep to stop after alpha", calls)
	}
}

// A 400 that reports an exhausted balance is not a request fault, so the sweep moves on to the next credential.
func TestUnifiedCreditsSweepContinuesPastCreditExhaustedBadRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			m, e, model := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
			e.plainStatus = http.StatusTooManyRequests
			e.creditsErrors = map[string]error{
				"alpha": &Error{HTTPStatus: http.StatusBadRequest, Message: `{"error":{"code":"insufficient_quota","message":"out of credits"}}`},
			}
			if err := runCreditsRoutingRequest(t, m, model, stream); err != nil {
				t.Fatalf("request failed: %v; calls = %s", err, fmt.Sprint(e.snapshot()))
			}
			if got := fmt.Sprint(e.snapshot()); got != "[alpha:plain beta:plain alpha:credits beta:credits]" {
				t.Fatalf("calls = %s, want the sweep to reach beta", got)
			}
		})
	}
}

// Terminal unauthorized state excludes a credential from credits for every model, not only unified ones.
// Cooldown from a plain 429 still does not exclude it.
func TestCreditsCandidatesExcludeTerminalCredentialForNonUnifiedModel(t *testing.T) {
	const model = "claude-plain-review"
	m, _, _ := newUnifiedCreditsRoutingManager(t, internalconfig.RoutingPolicy{Enabled: true})
	markUnifiedAuthTerminal(m, "alpha")
	m.mu.Lock()
	beta := m.auths["beta"]
	beta.Unavailable = true
	beta.Status = StatusError
	beta.LastError = &Error{Code: "rate_limited", HTTPStatus: http.StatusTooManyRequests, Message: "slow down"}
	beta.NextRetryAfter = time.Now().Add(time.Hour)
	m.mu.Unlock()
	candidates, err := m.findAllAntigravityCreditsCandidateAuths(context.Background(), model, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].auth.ID != "beta" {
		t.Fatalf("candidates = %+v, want only the cooled-down beta", candidates)
	}
	if m.unifiedCreditsEligible(model, "alpha") {
		t.Fatal("terminally rejected alpha is eligible for credits on a non-unified model")
	}
	if !m.unifiedCreditsEligible(model, "beta") {
		t.Fatal("cooldown excluded beta from credits on a non-unified model")
	}
}

// The plain round's marked upstream failure keeps precedence over a credits failure, even when the plain error is
// one of the synthetic answers. Without the marker the synthetic answer yields to the credits failure.
func TestPreferCreditsAttemptErrorHonorsUpstreamMarker(t *testing.T) {
	creditsErr := &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "busy"}
	marked := markUpstreamExecutionAttempt(newModelCooldownError("claude-plain-review", "antigravity", time.Minute))
	var cooldown *modelCooldownError
	if got := preferCreditsAttemptError(marked, creditsErr); !errors.As(got, &cooldown) {
		t.Fatalf("marked cooldown lost to credits: got %v", got)
	}
	if got := preferCreditsAttemptError(newModelCooldownError("claude-plain-review", "antigravity", time.Minute), creditsErr); got != creditsErr {
		t.Fatalf("unmarked cooldown kept over credits: got %v", got)
	}
}
