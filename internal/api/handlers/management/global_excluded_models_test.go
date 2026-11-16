package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestGlobalExcludedModelsV8ConfigRoute checks that the v8 config path reads and
// writes routing.excluded-models, and that the legacy root key still loads.
func TestGlobalExcludedModelsV8ConfigRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	// Existing files carry the legacy root key; it must stay readable.
	if err := os.WriteFile(path, []byte("port: 8317\nglobal-excluded-models: [\"vendor-a/*\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.GlobalExcludedModels; len(got) != 1 || got[0] != "vendor-a/*" {
		t.Fatalf("legacy key loaded as %v", got)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.GET("/v8/management/config/*path", h.ConfigV8)
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	do := func(method, url, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, url, strings.NewReader(body)))
		return w
	}

	if w := do(http.MethodGet, "/v8/management/config/routing/excluded-models", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "vendor-a/*") {
		t.Fatalf("v8 GET status=%d body=%s", w.Code, w.Body.String())
	}
	if w := do(http.MethodPut, "/v8/management/config/routing/excluded-models", `["*-preview"]`); w.Code != http.StatusOK {
		t.Fatalf("v8 PUT status=%d body=%s", w.Code, w.Body.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = config.ValidateV8Config(saved); err != nil {
		t.Fatalf("saved config is not valid v8: %v\n%s", err, saved)
	}
	reloaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.GlobalExcludedModels; len(got) != 1 || got[0] != "*-preview" {
		t.Fatalf("v8 write reloaded as %v\n%s", got, saved)
	}
}
