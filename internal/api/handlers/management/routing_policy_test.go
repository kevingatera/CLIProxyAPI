package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type managementRoutingTestExecutor struct {
	id string
}

func (e *managementRoutingTestExecutor) Identifier() string { return e.id }

func (e *managementRoutingTestExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	_ = opts
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *managementRoutingTestExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_ = ctx
	_ = auth
	_ = req
	_ = opts
	return nil, nil
}

func (e *managementRoutingTestExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *managementRoutingTestExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	_ = opts
	return cliproxyexecutor.Response{}, nil
}

func (e *managementRoutingTestExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	return nil, nil
}

func writeRoutingTestConfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("routing:\n  strategy: round-robin\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

func TestPutRoutingPolicy_ValidPayloadPersists(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.SanitizeRoutingPolicy()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&managementRoutingTestExecutor{id: "codex"})
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "codex-a", Provider: "codex", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	h := NewHandler(cfg, writeRoutingTestConfigFile(t), manager)

	body := map[string]any{
		"policy": map[string]any{
			"enabled": true,
			"defaults": map[string]any{
				"route": []map[string]any{
					{
						"provider":               "codex",
						"auth-order":             []string{"codex-a"},
						"include-remaining-auth": true,
					},
				},
			},
		},
	}
	payload, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/routing/policy", bytes.NewReader(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutRoutingPolicy(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !h.cfg.Routing.Policy.Enabled {
		t.Fatalf("policy enabled = false, want true")
	}
	if len(h.cfg.Routing.Policy.Defaults.Route) != 1 {
		t.Fatalf("route count = %d, want 1", len(h.cfg.Routing.Policy.Defaults.Route))
	}
}

func TestPutRoutingPolicy_RejectsAuthProviderMismatch(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&managementRoutingTestExecutor{id: "codex"})
	manager.RegisterExecutor(&managementRoutingTestExecutor{id: "claude"})
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "claude-a", Provider: "claude", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	h := NewHandler(cfg, writeRoutingTestConfigFile(t), manager)

	body := map[string]any{
		"policy": map[string]any{
			"enabled": true,
			"defaults": map[string]any{
				"route": []map[string]any{
					{
						"provider":   "codex",
						"auth-order": []string{"claude-a"},
					},
				},
			},
		},
	}
	payload, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/routing/policy", bytes.NewReader(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutRoutingPolicy(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestPreviewRoutingPolicy_ResolvesProvidersFromModel(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	model := "gpt-5.4-mini"
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			Policy: config.RoutingPolicy{
				Enabled: true,
				Defaults: config.RoutingPolicyRule{
					Route: []config.RoutingPolicyRoute{{Provider: "codex", IncludeRemainingAuth: true}},
				},
			},
		},
	}
	cfg.SanitizeRoutingPolicy()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(&managementRoutingTestExecutor{id: "codex"})
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "codex-a", Provider: "codex", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("codex-a", "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient("codex-a")
	})

	h := NewHandler(cfg, writeRoutingTestConfigFile(t), manager)
	payload, _ := json.Marshal(map[string]any{"model": model})
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/routing/preview", bytes.NewReader(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PreviewRoutingPolicy(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Preview coreauth.RoutingPreview `json:"preview"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	if len(response.Preview.OrderedProviders) == 0 || response.Preview.OrderedProviders[0] != "codex" {
		t.Fatalf("ordered providers = %v, want codex first", response.Preview.OrderedProviders)
	}
}
