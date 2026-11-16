package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestUsageSnapshotV8RoutesRequireManagementAuth checks the v8 export and import routes
// and confirms no v0 usage read is registered.
func TestUsageSnapshotV8RoutesRequireManagementAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nremote-management: {secret-key: test-password}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Plugins.Dir = filepath.Dir(path)
	h := management.NewHandler(cfg, path, nil)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()

	serve := func(method, route, body string, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, route, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		if authorized {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		rr := httptest.NewRecorder()
		s.engine.ServeHTTP(rr, req)
		return rr
	}

	const exportRoute = "/v8/management/observability/usage/export"
	const importRoute = "/v8/management/observability/usage/import"
	if rr := serve(http.MethodGet, exportRoute, "", false); rr.Code != http.StatusUnauthorized {
		t.Fatalf("export without key status = %d, want 401", rr.Code)
	}
	if rr := serve(http.MethodGet, exportRoute, "", true); rr.Code != http.StatusOK {
		t.Fatalf("export status = %d body=%s", rr.Code, rr.Body.String())
	} else if !bytes.Contains(rr.Body.Bytes(), []byte(`"usage"`)) {
		t.Fatalf("export body lacks usage envelope: %s", rr.Body.String())
	}
	if rr := serve(http.MethodPost, importRoute, `{"version":9,"usage":{}}`, true); rr.Code != http.StatusBadRequest {
		t.Fatalf("import of unsupported version status = %d, want 400", rr.Code)
	}
	if rr := serve(http.MethodGet, "/v0/management/usage", "", true); rr.Code != http.StatusNotFound {
		t.Fatalf("v0 usage route status = %d, want 404", rr.Code)
	}
}
