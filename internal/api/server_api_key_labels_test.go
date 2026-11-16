package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestAPIKeyLabelsV8RouteThroughServer checks the v8 contract through the real
// server: GET returns an ETag, a stale If-Match gets 412, and a current one saves.
func TestAPIKeyLabelsV8RouteThroughServer(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")

	server := newTestServer(t)
	if err := os.WriteFile(server.configFilePath, []byte("config-version: 8\nserver: {port: 8317}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := proxyconfig.LoadConfig(server.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	server.cfgMu.Lock()
	server.cfg = cfg
	server.cfgMu.Unlock()
	server.mgmt.SetConfig(cfg)

	do := func(method, body, ifMatch string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/v8/management/api-key-labels", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-management-key")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, req)
		return w
	}

	get := do(http.MethodGet, "", "")
	if get.Code != http.StatusOK || get.Header().Get("ETag") == "" {
		t.Fatalf("v8 GET status=%d etag=%q body=%s", get.Code, get.Header().Get("ETag"), get.Body.String())
	}
	etag := get.Header().Get("ETag")

	if w := do(http.MethodPut, `{"api-key-labels":{"key:sha256:0123456789abcdef":"One"}}`, etag); w.Code != http.StatusOK {
		t.Fatalf("v8 conditional PUT status=%d body=%s", w.Code, w.Body.String())
	}
	stale := do(http.MethodPut, `{"api-key-labels":{"key:sha256:0123456789abcdef":"Two"}}`, etag)
	if stale.Code != http.StatusPreconditionFailed || stale.Header().Get("ETag") == "" {
		t.Fatalf("stale v8 PUT status=%d etag=%q, want 412 with ETag", stale.Code, stale.Header().Get("ETag"))
	}
	if w := do(http.MethodGet, "", ""); !strings.Contains(w.Body.String(), `"key:sha256:0123456789abcdef":"One"`) {
		t.Fatalf("v8 GET after stale write body=%s", w.Body.String())
	}
}
