package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type routingPolicyCall struct {
	op      string
	authID  string
	model   string
	payload string
}

type routingPolicyTestExecutor struct {
	provider  string
	mu        sync.Mutex
	calls     []routingPolicyCall
	errs      map[string]error
	streamErr error
}

func (e *routingPolicyTestExecutor) Identifier() string { return e.provider }

func (e *routingPolicyTestExecutor) record(op string, auth *Auth, req cliproxyexecutor.Request) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, routingPolicyCall{op: op, authID: auth.ID, model: req.Model, payload: string(req.Payload)})
	return e.errs[auth.ID]
}

func (e *routingPolicyTestExecutor) snapshot() []routingPolicyCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]routingPolicyCall(nil), e.calls...)
}

func (e *routingPolicyTestExecutor) Execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.record("execute", auth, req); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte("completion:" + auth.ID)}, nil
}

func (e *routingPolicyTestExecutor) CountTokens(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.record("count", auth, req); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"input_tokens":1}`)}, nil
}

func (e *routingPolicyTestExecutor) ExecuteStream(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.record("stream", auth, req); err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("chunk:" + auth.ID)}
	if e.streamErr != nil {
		chunks <- cliproxyexecutor.StreamChunk{Err: e.streamErr}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (*routingPolicyTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*routingPolicyTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newRoutingPolicyTestManager(t *testing.T, name string, authIDs []string, policy internalconfig.RoutingPolicy) (*Manager, *routingPolicyTestExecutor, string) {
	t.Helper()
	provider := "routing-policy-" + name
	model := provider + "-model"
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	executor := &routingPolicyTestExecutor{provider: provider, errs: map[string]error{}}
	manager.RegisterExecutor(executor)
	limits := make(map[string]int, len(authIDs))
	for _, id := range authIDs {
		limits[provider+"-"+id] = 0
	}
	registerRetryRoundLocalAuths(t, manager, provider, model, limits)
	for i := range policy.Defaults.Route {
		policy.Defaults.Route[i].Provider = provider
		for j, id := range policy.Defaults.Route[i].AuthOrder {
			policy.Defaults.Route[i].AuthOrder[j] = provider + "-" + id
		}
	}
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Policy: policy}})
	return manager, executor, model
}

func routeIDs(calls []routingPolicyCall, provider string) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, call.op+":"+call.authID[len(provider)+1:])
	}
	return out
}

func assertRouteCalls(t *testing.T, executor *routingPolicyTestExecutor, want ...string) {
	t.Helper()
	got := routeIDs(executor.snapshot(), executor.provider)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func drainStream(t *testing.T, result *cliproxyexecutor.StreamResult) error {
	t.Helper()
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	return streamErr
}

func pinnedPolicy(authOrder []string, includeRemaining bool, fallbackOn ...string) internalconfig.RoutingPolicy {
	return internalconfig.RoutingPolicy{
		Enabled:  true,
		Defaults: internalconfig.RoutingPolicyRule{Route: []internalconfig.RoutingPolicyRoute{{AuthOrder: authOrder, IncludeRemainingAuth: includeRemaining}}},
		Fallback: internalconfig.RoutingFallbackPolicy{On: fallbackOn},
	}
}

// A token-count request dispatches CountTokens on every candidate and never Execute.
func TestRoutingPolicyCountTokensNeverExecutes(t *testing.T) {
	manager, executor, model := newRoutingPolicyTestManager(t, "count", []string{"a", "b"}, pinnedPolicy([]string{"b", "a"}, false))
	executor.errs[executor.provider+"-b"] = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limited"}

	resp, err := manager.ExecuteCount(context.Background(), []string{executor.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteCount: %v", err)
	}
	if string(resp.Payload) != `{"input_tokens":1}` {
		t.Fatalf("payload = %s, want token count", resp.Payload)
	}
	assertRouteCalls(t, executor, "count:b", "count:a")
}

// Pinned candidates run through the normal pipeline, so request-after-auth interceptors see the body.
func TestRoutingPolicyCandidatesUseNormalPipeline(t *testing.T) {
	manager, executor, model := newRoutingPolicyTestManager(t, "pipeline", []string{"a", "b"}, pinnedPolicy([]string{"b", "a"}, false))
	intercepted := 0
	opts := cliproxyexecutor.Options{
		RequestAfterAuthInterceptor: func(_ context.Context, in cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
			intercepted++
			return cliproxyexecutor.RequestAfterAuthInterceptResponse{Body: []byte("intercepted")}
		},
	}

	resp, err := manager.Execute(context.Background(), []string{executor.provider}, cliproxyexecutor.Request{Model: model, Payload: []byte("original")}, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(resp.Payload) != "completion:"+executor.provider+"-b" {
		t.Fatalf("payload = %s, want first pinned auth b", resp.Payload)
	}
	calls := executor.snapshot()
	if intercepted != 1 || len(calls) != 1 || calls[0].payload != "intercepted" {
		t.Fatalf("interceptor ran %d times, calls = %+v; want the pinned call to see the intercepted body", intercepted, calls)
	}
}

// A failure the policy does not allow to fall back stops the plan.
func TestRoutingPolicyStopsOnDisallowedFallback(t *testing.T) {
	manager, executor, model := newRoutingPolicyTestManager(t, "stop", []string{"a", "b"}, pinnedPolicy([]string{"a", "b"}, false, "rate_limited"))
	executor.errs[executor.provider+"-a"] = &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "down"}

	_, err := manager.Execute(context.Background(), []string{executor.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if statusCodeFromError(err) != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want the 503 from auth a", err)
	}
	assertRouteCalls(t, executor, "execute:a")
}

// With include-remaining-auth false, omitted auths are never used on any operation,
// even after every listed auth fails.
func TestRoutingPolicyNeverUsesOmittedAuths(t *testing.T) {
	ops := map[string]func(*Manager, string, string) error{
		"execute": func(m *Manager, provider, model string) error {
			_, err := m.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		},
		"count": func(m *Manager, provider, model string) error {
			_, err := m.ExecuteCount(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		},
		"stream": func(m *Manager, provider, model string) error {
			_, err := m.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
			return err
		},
	}
	for op, run := range ops {
		t.Run(op, func(t *testing.T) {
			manager, executor, model := newRoutingPolicyTestManager(t, "omit-"+op, []string{"a", "b", "c"}, pinnedPolicy([]string{"b"}, false))
			executor.errs[executor.provider+"-b"] = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limited"}

			if err := run(manager, executor.provider, model); statusCodeFromError(err) != http.StatusTooManyRequests {
				t.Fatalf("err = %v, want 429 from b", err)
			}
			assertRouteCalls(t, executor, op+":b")
		})
	}
}

// include-remaining-auth tries the other auths after the listed ones, without retrying the listed auth.
func TestRoutingPolicyIncludeRemainingAuthSkipsListed(t *testing.T) {
	manager, executor, model := newRoutingPolicyTestManager(t, "remaining", []string{"a", "b"}, pinnedPolicy([]string{"b"}, true))
	executor.errs[executor.provider+"-b"] = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limited"}

	if _, err := manager.Execute(context.Background(), []string{executor.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertRouteCalls(t, executor, "execute:b", "execute:a")
}

// Explicit order is honored for streams, and a stream that fails after it starts is traced as failed.
func TestRoutingPolicyStreamOrderAndCompletionTrace(t *testing.T) {
	manager, executor, model := newRoutingPolicyTestManager(t, "stream", []string{"a", "b", "c"}, pinnedPolicy([]string{"c", "a"}, false))
	executor.errs[executor.provider+"-c"] = &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"}
	executor.streamErr = &Error{HTTPStatus: http.StatusBadGateway, Message: "reset mid-stream"}

	result, err := manager.ExecuteStream(context.Background(), []string{executor.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if streamErr := drainStream(t, result); streamErr == nil {
		t.Fatal("expected the mid-stream error to reach the client")
	}
	assertRouteCalls(t, executor, "stream:c", "stream:a")

	traces := manager.ListRoutingTraces(10, "", "", false)
	if len(traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(traces))
	}
	trace := traces[0]
	if trace.FinalStatus != "failed" || len(trace.Attempts) != 2 || trace.Attempts[0].FallbackReason != "server_error" || !trace.Attempts[1].Success {
		t.Fatalf("trace = %+v, want c server_error fallback, a started, final failed", trace)
	}
}

// Traces are recorded only while the policy is enabled.
func TestRoutingPolicyTracesOnlyWhenEnabled(t *testing.T) {
	manager, executor, model := newRoutingPolicyTestManager(t, "trace-off", []string{"a"}, internalconfig.RoutingPolicy{})
	if _, err := manager.Execute(context.Background(), []string{executor.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if traces := manager.ListRoutingTraces(10, "", "", false); len(traces) != 0 {
		t.Fatalf("disabled policy recorded %d traces", len(traces))
	}

	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Policy: internalconfig.RoutingPolicy{Enabled: true}}})
	if _, err := manager.Execute(context.Background(), []string{executor.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	traces := manager.ListRoutingTraces(10, "", "", false)
	if len(traces) != 1 || traces[0].FinalStatus != "success" {
		t.Fatalf("enabled policy traces = %+v, want one success", traces)
	}
}

// Fallback classification uses structured signals only; error text never matters.
func TestClassifyRoutingFailureUsesStructuredSignals(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"quota text without status", context.Background(), errors.New("quota exhausted, cooldown"), ""},
		{"bare eof text", context.Background(), errors.New("unexpected eof from upstream"), ""},
		{"402", context.Background(), &Error{HTTPStatus: http.StatusPaymentRequired}, "exhausted"},
		{"model cooldown", context.Background(), newModelCooldownError("m", "p", 0), "exhausted"},
		{"429", context.Background(), &Error{HTTPStatus: http.StatusTooManyRequests}, "rate_limited"},
		{"503", context.Background(), &Error{HTTPStatus: http.StatusServiceUnavailable}, "server_error"},
		{"401", context.Background(), &Error{HTTPStatus: http.StatusUnauthorized}, ""},
		{"net error", context.Background(), &net.OpError{Op: "dial", Err: errors.New("refused")}, "transport_error"},
		{"wrapped unexpected eof", context.Background(), fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), "transport_error"},
		{"unknown auth", context.Background(), &Error{Code: "auth_not_found", Message: "no auth available"}, "unavailable"},
		{"request canceled", canceled, &Error{HTTPStatus: http.StatusServiceUnavailable}, ""},
	}
	for _, tc := range cases {
		if got := classifyRoutingFailure(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRoutingTraceRingKeepsNewest(t *testing.T) {
	var ring routingTraceRing
	for i := 1; i <= 5; i++ {
		ring.add(RoutingTrace{ID: fmt.Sprint(i)}, 3)
	}
	if len(ring.buf) != 3 {
		t.Fatalf("buffer len = %d, want fixed capacity 3", len(ring.buf))
	}
	ids := func() string {
		out := ""
		for _, trace := range ring.orderedLocked() {
			out += trace.ID
		}
		return out
	}
	if got := ids(); got != "345" {
		t.Fatalf("ordered = %s, want 345", got)
	}
	ring.add(RoutingTrace{ID: "6"}, 2)
	if got := ids(); got != "56" {
		t.Fatalf("after shrink ordered = %s, want 56", got)
	}
	ring.add(RoutingTrace{ID: "7"}, 4)
	if got := ids(); got != "567" {
		t.Fatalf("after grow ordered = %s, want 567", got)
	}
}

// The shared request retry credential budget spans the whole plan, not each route step.
func TestRoutingPolicyRetryCredentialBudgetSpansPlan(t *testing.T) {
	manager, executor, model := newRoutingPolicyTestManager(t, "budget", []string{"a", "b", "c"}, pinnedPolicy([]string{"a", "b"}, true))
	manager.SetRetryConfig(0, 0, 2)
	for _, id := range []string{"a", "b", "c"} {
		executor.errs[executor.provider+"-"+id] = &Error{HTTPStatus: http.StatusInternalServerError, Message: "down"}
	}

	if _, err := manager.Execute(context.Background(), []string{executor.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); statusCodeFromError(err) != http.StatusInternalServerError {
		t.Fatalf("err = %v, want the 500 from the last attempted auth", err)
	}
	assertRouteCalls(t, executor, "execute:a", "execute:b")
}

// Invalid requests with no providers never reach the plan and leave no trace.
func TestRoutingPolicyNoProvidersLeaveNoTrace(t *testing.T) {
	manager, _, model := newRoutingPolicyTestManager(t, "no-providers", []string{"a"}, pinnedPolicy([]string{"a"}, false))
	_, err := manager.ExecuteStream(context.Background(), nil, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "provider_not_found" {
		t.Fatalf("ExecuteStream err = %v, want provider_not_found", err)
	}
	if _, err := manager.Execute(context.Background(), nil, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); !errors.As(err, &authErr) || authErr.Code != "provider_not_found" {
		t.Fatalf("Execute err = %v, want provider_not_found", err)
	}
	if traces := manager.ListRoutingTraces(10, "", "", false); len(traces) != 0 {
		t.Fatalf("invalid requests recorded %d traces", len(traces))
	}
}

// Pinned candidates run through the normal pipeline, so model aliases resolve to the upstream model.
func TestRoutingPolicyCandidatesResolveModelAliases(t *testing.T) {
	manager, executor, model := newRoutingPolicyTestManager(t, "alias", []string{"a", "b"}, pinnedPolicy([]string{"b"}, false))
	alias := model + "-alias"
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		executor.provider: {{Name: model, Alias: alias, Fork: true}},
	})
	reg := registry.GetGlobalRegistry()
	for _, id := range []string{"a", "b"} {
		authID := executor.provider + "-" + id
		reg.RegisterClient(authID, executor.provider, []*registry.ModelInfo{{ID: alias}, {ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(authID) })
		manager.RefreshSchedulerEntry(authID)
	}

	if _, err := manager.Execute(context.Background(), []string{executor.provider}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	calls := executor.snapshot()
	if len(calls) != 1 || calls[0].authID != executor.provider+"-b" || calls[0].model != model {
		t.Fatalf("calls = %+v, want one pinned call to b with upstream model %s", calls, model)
	}
}
