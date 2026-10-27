package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type routingIdentityExecutor struct {
	*routingPolicyTestExecutor
	observe func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options)
}

func (e *routingIdentityExecutor) Execute(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.observe(ctx, req, opts)
	return e.routingPolicyTestExecutor.Execute(ctx, a, req, opts)
}

func (e *routingIdentityExecutor) CountTokens(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.observe(ctx, req, opts)
	return e.routingPolicyTestExecutor.CountTokens(ctx, a, req, opts)
}

func (e *routingIdentityExecutor) ExecuteStream(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.observe(ctx, req, opts)
	return e.routingPolicyTestExecutor.ExecuteStream(ctx, a, req, opts)
}

func TestRoutingPolicyFallbackPreservesRequestIdentity(t *testing.T) {
	for _, op := range []string{"execute", "count", "stream"} {
		t.Run(op, func(t *testing.T) {
			m, e, model := newRoutingPolicyTestManager(t, "identity-"+op, []string{"a", "b"}, pinnedPolicy([]string{"a", "b"}, false))
			e.errs[e.provider+"-a"] = &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "retry elsewhere"}
			seen := 0
			m.RegisterExecutor(&routingIdentityExecutor{routingPolicyTestExecutor: e, observe: func(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
				seen++
				if !coreusage.HasCapturedIdentity(ctx) || coreusage.APIKeyFromContext(ctx) != "synthetic-client" || coreusage.AccessProviderFromContext(ctx) != "synthetic-access" {
					t.Fatal("routing lost the captured usage identity")
				}
				if !IsCodexClientRequest(ctx) {
					t.Fatal("routing lost the captured Codex client flag")
				}
				if coreusage.ExecutionRequestIDFromContext(ctx) != "routing-request" || coreusage.TraceIDFromContext(ctx) != "routing-trace" {
					t.Fatal("routing changed the execution request or trace identity")
				}
				if opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] != "header:routing-session" || req.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] != "header:routing-session" {
					t.Fatal("routing lost the canonical session identity")
				}
				if req.Model != model || coreusage.RequestedModelAliasFromContext(ctx) != model {
					t.Fatal("routing lost the requested model identity")
				}
			}})
			ctx := coreusage.WithAPIKey(context.Background(), "synthetic-client")
			ctx = coreusage.WithAccessProvider(ctx, "synthetic-access")
			ctx = coreusage.WithExecutionRequestID(ctx, "routing-request")
			ctx = coreusage.WithTraceID(ctx, "routing-trace")
			opts := cliproxyexecutor.Options{Headers: http.Header{"User-Agent": {"codex_cli_rs/0.137.0"}, "X-Session-Id": {"routing-session"}}}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"synthetic"}`)}
			var err error
			switch op {
			case "execute":
				_, err = m.Execute(ctx, []string{e.provider}, req, opts)
			case "count":
				_, err = m.ExecuteCount(ctx, []string{e.provider}, req, opts)
			case "stream":
				opts.Stream = true
				var result *cliproxyexecutor.StreamResult
				result, err = m.ExecuteStream(ctx, []string{e.provider}, req, opts)
				if err == nil {
					err = drainStream(t, result)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if seen != 2 {
				t.Fatalf("attempts = %d, want 2", seen)
			}
			assertRouteCalls(t, e, op+":a", op+":b")
		})
	}
}

func TestRoutingPolicyOuterRetryKeepsAuthRestrictions(t *testing.T) {
	m, e, model := newRoutingPolicyTestManager(t, "outer-retry", []string{"a", "omitted"}, pinnedPolicy([]string{"a"}, false))
	m.SetRetryConfig(1, 0, 0)
	a, _ := m.GetByID(e.provider + "-a")
	a.Metadata["request_retry"] = 1
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	seen := 0
	e.errs[a.ID] = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}
	m.RegisterExecutor(&routingIdentityExecutor{routingPolicyTestExecutor: e, observe: func(_ context.Context, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
		seen++
		if pinnedAuthIDFromMetadata(opts.Metadata) != a.ID {
			t.Fatal("outer retry escaped the auth restriction")
		}
		if seen == 2 {
			delete(e.errs, a.ID)
		}
	}})
	if _, err := m.Execute(context.Background(), []string{e.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatal(err)
	}
	assertRouteCalls(t, e, "execute:a", "execute:a")
}
