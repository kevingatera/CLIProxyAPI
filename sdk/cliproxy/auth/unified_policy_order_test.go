package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	executor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// explicitOrderPolicy sets an enabled routing policy whose default rule pins the auth order of one provider.
func explicitOrderPolicy(provider string, includeRemaining bool, authIDs ...string) config.RoutingPolicy {
	return config.RoutingPolicy{Enabled: true, Defaults: config.RoutingPolicyRule{Route: []config.RoutingPolicyRoute{
		{Provider: provider, AuthOrder: authIDs, IncludeRemainingAuth: includeRemaining},
	}}}
}

func planAuthIDs(t *testing.T, m *Manager, operation string) []string {
	t.Helper()
	plan := m.routingPlanFor(operation, []string{"unified-go"}, "cliproxy/unified-test", executor.Options{})
	if plan == nil {
		t.Fatal("nil plan")
	}
	ids := make([]string, 0, len(plan.steps))
	for _, step := range plan.steps {
		ids = append(ids, step.authID)
	}
	return ids
}

func callAuthIDs(calls []routingPolicyCall) []string {
	ids := make([]string, 0, len(calls))
	for _, call := range calls {
		ids = append(ids, call.authID)
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An explicit auth-order is authoritative: rotation never reorders it, on plans and on real executions.
func TestUnifiedExplicitAuthOrderIsAuthoritative(t *testing.T) {
	m, exec := setupUnifiedTwinsWithPolicy(t, explicitOrderPolicy("unified-go", false, "unified-go-b", "unified-go-a"), "unified-go-a", "unified-go-b")
	req := executor.Request{Model: "cliproxy/unified-test"}
	for range 4 {
		if got := planAuthIDs(t, m, "execute"); !equalStrings(got, []string{"unified-go-b", "unified-go-a"}) {
			t.Fatalf("plan = %v, want the configured order", got)
		}
	}
	for range 4 {
		if _, err := m.Execute(context.Background(), []string{"unified-go"}, req, executor.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := callAuthIDs(exec.snapshot()); !equalStrings(got, []string{"unified-go-b", "unified-go-b", "unified-go-b", "unified-go-b"}) {
		t.Fatalf("executions = %v, want every request on the first configured auth", got)
	}
	if len(m.unifiedRotation) != 0 {
		t.Fatalf("explicit order recorded rotation state: %+v", m.unifiedRotation)
	}
}

// With include-remaining-auth, the listed auth leads and the unlisted one is only a fallback. A failure
// falls back to the unlisted auth, which is never tried ahead of the listed one. The failed listed auth
// then cools down, so later plans skip it.
func TestUnifiedExplicitAuthOrderFallsBackInsideTheList(t *testing.T) {
	m, exec := setupUnifiedTwinsWithPolicy(t, explicitOrderPolicy("unified-go", true, "unified-go-b"), "unified-go-a", "unified-go-b")
	exec.errs["unified-go-b"] = &Error{HTTPStatus: 503, Message: "down"}
	req := executor.Request{Model: "cliproxy/unified-test"}
	if got := planAuthIDs(t, m, "execute"); !equalStrings(got, []string{"unified-go-b", "unified-go-a"}) {
		t.Fatalf("plan = %v, want listed b before remaining a", got)
	}
	if _, err := m.Execute(context.Background(), []string{"unified-go"}, req, executor.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := planAuthIDs(t, m, "execute"); !equalStrings(got, []string{"unified-go-a"}) {
		t.Fatalf("plan after b failed = %v, want b cooled and skipped", got)
	}
	if _, err := m.Execute(context.Background(), []string{"unified-go"}, req, executor.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := callAuthIDs(exec.snapshot()); !equalStrings(got, []string{"unified-go-b", "unified-go-a", "unified-go-a"}) {
		t.Fatalf("executions = %v, want b, then a fallback, then a", got)
	}
}

// Without include-remaining-auth, unlisted auths are never planned or executed, even after the listed one fails.
func TestUnifiedExplicitAuthOrderExcludesUnlistedAuths(t *testing.T) {
	m, exec := setupUnifiedTwinsWithPolicy(t, explicitOrderPolicy("unified-go", false, "unified-go-b"), "unified-go-a", "unified-go-b")
	exec.errs["unified-go-b"] = &Error{HTTPStatus: 503, Message: "down"}
	if got := planAuthIDs(t, m, "execute"); !equalStrings(got, []string{"unified-go-b"}) {
		t.Fatalf("plan = %v, want only the listed auth", got)
	}
	if _, err := m.Execute(context.Background(), []string{"unified-go"}, executor.Request{Model: "cliproxy/unified-test"}, executor.Options{}); err == nil {
		t.Fatal("expected the listed auth's failure to surface")
	}
	if got := callAuthIDs(exec.snapshot()); !equalStrings(got, []string{"unified-go-b"}) {
		t.Fatalf("executions = %v, want only the listed auth", got)
	}
}

// A listed auth in credential cooldown is skipped, and the next listed auth serves the request.
func TestUnifiedExplicitAuthOrderSkipsCooledAuth(t *testing.T) {
	m, exec := setupUnifiedTwinsWithPolicy(t, explicitOrderPolicy("unified-go", false, "unified-go-b", "unified-go-a"), "unified-go-a", "unified-go-b")
	m.mu.Lock()
	m.auths["unified-go-b"].Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Hour)}
	m.mu.Unlock()
	if got := planAuthIDs(t, m, "execute"); !equalStrings(got, []string{"unified-go-a"}) {
		t.Fatalf("plan = %v, want the cooled auth skipped", got)
	}
	if _, err := m.Execute(context.Background(), []string{"unified-go"}, executor.Request{Model: "cliproxy/unified-test"}, executor.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := callAuthIDs(exec.snapshot()); !equalStrings(got, []string{"unified-go-a"}) {
		t.Fatalf("executions = %v, want only the uncooled auth", got)
	}
}

// The explicit order holds on the streaming path. Its fallback inside the list works the same way,
// and the failed listed auth is then cooled and skipped.
func TestUnifiedExplicitAuthOrderHoldsOnStreams(t *testing.T) {
	m, exec := setupUnifiedTwinsWithPolicy(t, explicitOrderPolicy("unified-go", true, "unified-go-b"), "unified-go-a", "unified-go-b")
	exec.errs["unified-go-b"] = &Error{HTTPStatus: 503, Message: "down"}
	req := executor.Request{Model: "cliproxy/unified-test"}
	for range 2 {
		stream, err := m.ExecuteStream(context.Background(), []string{"unified-go"}, req, executor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for range stream.Chunks {
		}
	}
	if got := callAuthIDs(exec.snapshot()); !equalStrings(got, []string{"unified-go-b", "unified-go-a", "unified-go-a"}) {
		t.Fatalf("stream executions = %v, want b, then a fallback, then a", got)
	}
}

// Token counting reads the explicit order without any effect on rotation or executions.
func TestUnifiedExplicitAuthOrderCountTokensReadOnly(t *testing.T) {
	m, exec := setupUnifiedTwinsWithPolicy(t, explicitOrderPolicy("unified-go", false, "unified-go-b", "unified-go-a"), "unified-go-a", "unified-go-b")
	for range 3 {
		if got := planAuthIDs(t, m, "count_tokens"); !equalStrings(got, []string{"unified-go-b", "unified-go-a"}) {
			t.Fatalf("count_tokens plan = %v", got)
		}
	}
	if len(m.unifiedRotation) != 0 || len(exec.snapshot()) != 0 {
		t.Fatalf("count_tokens changed state: rotation %+v calls %+v", m.unifiedRotation, exec.snapshot())
	}
}
