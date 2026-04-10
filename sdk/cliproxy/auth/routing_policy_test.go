package auth

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type routingPolicyTestExecutor struct {
	id string

	execute func(*Auth) error
	calls   []string
}

func (e *routingPolicyTestExecutor) Identifier() string { return e.id }

func (e *routingPolicyTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = req
	_ = opts
	if auth != nil {
		e.calls = append(e.calls, auth.ID)
	}
	if e.execute != nil {
		if err := e.execute(auth); err != nil {
			return cliproxyexecutor.Response{}, err
		}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *routingPolicyTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_ = ctx
	_ = auth
	_ = req
	_ = opts
	return nil, nil
}

func (e *routingPolicyTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *routingPolicyTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	_ = opts
	return cliproxyexecutor.Response{}, nil
}

func (e *routingPolicyTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	return nil, nil
}

func registerRoutingPolicyAuthModel(t *testing.T, provider, model string, authIDs ...string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			reg.UnregisterClient(authID)
		}
	})
}

func TestPreviewRouting_PolicyOrderAndAuthOrder(t *testing.T) {
	t.Parallel()

	model := "gpt-5.4-mini"
	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Policy: internalconfig.RoutingPolicy{
				Enabled: true,
				Defaults: internalconfig.RoutingPolicyRule{
					Route: []internalconfig.RoutingPolicyRoute{
						{Provider: "claude", AuthOrder: []string{"claude-2", "claude-1"}},
						{Provider: "codex", IncludeRemainingAuth: true},
					},
					IncludeRemainingProviders: true,
				},
			},
		},
	}
	cfg.SanitizeRoutingPolicy()

	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.SetConfig(cfg)
	m.RegisterExecutor(&routingPolicyTestExecutor{id: "claude"})
	m.RegisterExecutor(&routingPolicyTestExecutor{id: "codex"})

	for _, auth := range []*Auth{
		{ID: "claude-1", Provider: "claude", Status: StatusActive},
		{ID: "claude-2", Provider: "claude", Status: StatusActive},
		{ID: "codex-a", Provider: "codex", Status: StatusActive},
	} {
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}
	registerRoutingPolicyAuthModel(t, "claude", model, "claude-1", "claude-2")
	registerRoutingPolicyAuthModel(t, "codex", model, "codex-a")

	preview := m.PreviewRouting(model, []string{"codex", "claude"}, cliproxyexecutor.Options{})
	if !preview.PolicyEnabled {
		t.Fatalf("preview policy-enabled = false, want true")
	}
	if len(preview.OrderedProviders) != 2 || preview.OrderedProviders[0] != "claude" || preview.OrderedProviders[1] != "codex" {
		t.Fatalf("ordered providers = %v, want [claude codex]", preview.OrderedProviders)
	}
	if len(preview.ProviderDetails) != 2 {
		t.Fatalf("provider details len = %d, want 2", len(preview.ProviderDetails))
	}
	if got := preview.ProviderDetails[0].AuthOrder; len(got) != 2 || got[0] != "claude-2" || got[1] != "claude-1" {
		t.Fatalf("claude auth order = %v, want [claude-2 claude-1]", got)
	}
}

func TestExecute_PolicyStopsOnAuthMismatchStyleErrors(t *testing.T) {
	t.Parallel()

	model := "gpt-5.4-mini"
	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Policy: internalconfig.RoutingPolicy{
				Enabled: true,
				Defaults: internalconfig.RoutingPolicyRule{
					Route: []internalconfig.RoutingPolicyRoute{
						{Provider: "codex", AuthOrder: []string{"auth-a", "auth-b"}},
					},
				},
				Fallback: internalconfig.RoutingFallbackPolicy{
					On: []string{"exhausted", "rate_limited", "server_error", "transport_error"},
				},
			},
		},
	}
	cfg.SanitizeRoutingPolicy()

	exec := &routingPolicyTestExecutor{
		id: "codex",
		execute: func(auth *Auth) error {
			if auth != nil && auth.ID == "auth-a" {
				return &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
			}
			return nil
		},
	}
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.SetConfig(cfg)
	m.RegisterExecutor(exec)
	for _, auth := range []*Auth{
		{ID: "auth-a", Provider: "codex", Status: StatusActive},
		{ID: "auth-b", Provider: "codex", Status: StatusActive},
	} {
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}
	registerRoutingPolicyAuthModel(t, "codex", model, "auth-a", "auth-b")

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatalf("expected execute error")
	}
	if len(exec.calls) != 1 || exec.calls[0] != "auth-a" {
		t.Fatalf("executor calls = %v, want only auth-a", exec.calls)
	}
}

func TestExecute_RoutingTraceIncludesFallbackAttempts(t *testing.T) {
	t.Parallel()

	model := "gpt-5.4-mini"
	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Policy: internalconfig.RoutingPolicy{
				Enabled: true,
				Defaults: internalconfig.RoutingPolicyRule{
					Route: []internalconfig.RoutingPolicyRoute{
						{Provider: "codex", AuthOrder: []string{"auth-a", "auth-b"}},
					},
				},
				Fallback: internalconfig.RoutingFallbackPolicy{
					On: []string{"exhausted", "rate_limited", "server_error", "transport_error"},
				},
				Observability: internalconfig.RoutingPolicyObservability{TraceLimit: 32},
			},
		},
	}
	cfg.SanitizeRoutingPolicy()

	exec := &routingPolicyTestExecutor{
		id: "codex",
		execute: func(auth *Auth) error {
			if auth != nil && auth.ID == "auth-a" {
				return &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}
			}
			return nil
		},
	}
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.SetConfig(cfg)
	m.RegisterExecutor(exec)
	for _, auth := range []*Auth{
		{ID: "auth-a", Provider: "codex", Status: StatusActive},
		{ID: "auth-b", Provider: "codex", Status: StatusActive},
	} {
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}
	registerRoutingPolicyAuthModel(t, "codex", model, "auth-a", "auth-b")

	if _, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("execute should succeed on fallback auth: %v", err)
	}

	traces := m.ListRoutingTraces(5, model, "codex", false)
	if len(traces) == 0 {
		t.Fatalf("expected at least one routing trace")
	}
	latest := traces[0]
	if latest.FinalStatus != "success" {
		t.Fatalf("final status = %q, want success", latest.FinalStatus)
	}
	if len(latest.Attempts) < 2 {
		t.Fatalf("attempt count = %d, want >= 2", len(latest.Attempts))
	}
	firstAttempt := latest.Attempts[0]
	if !firstAttempt.Fallback {
		t.Fatalf("first attempt fallback = false, want true")
	}
	if firstAttempt.FallbackReason != "rate_limited" && firstAttempt.FallbackReason != "exhausted" {
		t.Fatalf("first attempt fallback reason = %q, want rate_limited/exhausted", firstAttempt.FallbackReason)
	}
}
