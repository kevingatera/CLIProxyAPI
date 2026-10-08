package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestMetaExecutorRejectsInactiveSubscriptionAfterMint(t *testing.T) {
	for _, path := range []string{"execute", "stream", "count"} {
		t.Run(path, func(t *testing.T) {
			inferenceCalls := 0
			mintCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/key" {
					mintCalls++
					_ = json.NewEncoder(w).Encode(map[string]any{"api_key": "test-key", "is_subs_active": false})
					return
				}
				inferenceCalls++
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			t.Setenv("META_MINT_URL", server.URL+"/key")
			cfg := &config.Config{}
			cfg.Routing.UnifiedModels = config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{{ID: "muse-test", Routes: []config.UnifiedModelRoute{{Provider: "meta", AuthKind: "oauth", SubscriptionOnly: true}}}}}
			executor := NewMetaExecutor(cfg)
			auth := &cliproxyauth.Auth{Provider: "meta", Metadata: map[string]any{"dca_token": "dca:test-" + path, "is_subs_active": true}, Attributes: map[string]string{"base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "muse-test", Payload: []byte(`{"model":"muse-test","messages":[]}`)}
			opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "cliproxy/muse-test(low)"}}
			var err error
			switch path {
			case "execute":
				_, err = executor.Execute(context.Background(), auth, req, opts)
			case "stream":
				_, err = executor.ExecuteStream(context.Background(), auth, req, opts)
			case "count":
				_, err = executor.CountTokens(context.Background(), auth, req, opts)
			}
			var status statusErr
			if !errors.As(err, &status) || status.code != http.StatusPaymentRequired {
				t.Fatalf("expected subscription rejection, got %v", err)
			}
			if mintCalls != 1 || inferenceCalls != 0 {
				t.Fatalf("mint calls %d, inference calls %d", mintCalls, inferenceCalls)
			}
		})
	}
}
