package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestAPIKeyLabelsV8RouteAndV0Compat checks the v8 contract through the real
// server and that the one-release /v0 alias still reads and writes the same store.
func TestAPIKeyLabelsV8RouteAndV0Compat(t *testing.T) {
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

	do := func(method, path, body string, ifMatch string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-management-key")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, req)
		return w
	}

	const v8 = "/v8/management/api-key-labels"
	const v0 = "/v0/management/api-key-labels"

	get := do(http.MethodGet, v8, "", "")
	if get.Code != http.StatusOK || get.Header().Get("ETag") == "" {
		t.Fatalf("v8 GET status=%d etag=%q body=%s", get.Code, get.Header().Get("ETag"), get.Body.String())
	}
	etag := get.Header().Get("ETag")

	if w := do(http.MethodPut, v8, `{"api-key-labels":{"abc":"One"}}`, etag); w.Code != http.StatusOK {
		t.Fatalf("v8 conditional PUT status=%d body=%s", w.Code, w.Body.String())
	}
	stale := do(http.MethodPut, v8, `{"api-key-labels":{"abc":"Two"}}`, etag)
	if stale.Code != http.StatusPreconditionFailed || stale.Header().Get("ETag") == "" {
		t.Fatalf("stale v8 PUT status=%d etag=%q, want 412 with ETag", stale.Code, stale.Header().Get("ETag"))
	}

	if w := do(http.MethodGet, v0, "", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"abc":"One"`) {
		t.Fatalf("v0 compat GET status=%d body=%s", w.Code, w.Body.String())
	}
}
