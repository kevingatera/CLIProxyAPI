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
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAllowanceCollectorScope(t *testing.T) {
	key := "synthetic-collector-token-at-least-32-characters"
	cfg := &config.Config{}
	h := &Handler{cfg: cfg, authManager: coreauth.NewManager(nil, nil, nil)}
	router := gin.New()
	router.GET("/capacity/v1/collect", h.CollectAllowances)
	for _, test := range []struct {
		configured, token string
		want              int
	}{
		{"", key, 404}, {key, "", 401}, {key, "management-token", 401}, {key, key, 200},
	} {
		cfg.Routing.Allowances.CollectorKey = test.configured
		r := httptest.NewRequest(http.MethodGet, "/capacity/v1/collect", nil)
		r.Header.Set("Authorization", "Bearer "+test.token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("expected %d, got %d", test.want, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("account response is cacheable")
		}
	}
}

func TestAllowanceKeysExcludedFromConfigJSON(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Allowances = config.AllowancesConfig{CollectorKey: "secret-collector", ServiceKey: "secret-reader", ServiceURL: "http://private.example/v1/allowances"}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.JSON(200, cfg)
	if strings.Contains(w.Body.String(), "secret-") || strings.Contains(w.Body.String(), "private.example") {
		t.Fatal("private allowance configuration leaked")
	}
}

func TestSharedNativeAccountMatchesOnlySameProviderAndKey(t *testing.T) {
	a := &coreauth.Auth{ID: "a", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://api.commandcode.ai/provider"}}
	b := a.Clone()
	b.ID = "b"
	if !sameNativeAccount(a, b) {
		t.Fatal("shared credential not recognized")
	}
	b.Attributes["base_url"] = "https://api.kimi.com/coding/v1"
	if sameNativeAccount(a, b) {
		t.Fatal("provider scopes conflated")
	}
}

// The v8 config route reads raw YAML, so secrets must be hidden there, not only by json:"-".
func TestConfigV8KeepsAllowanceSecretsPrivate(t *testing.T) {
	const collector = "synthetic-collector-token-at-least-32-characters"
	const reader = "synthetic-service-reader-token-at-least-32-chars"
	const serviceURL = "http://allowance.example.invalid/v1/allowances"
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := "port: 8317\nrouting:\n  strategy: round-robin\n  allowances:\n    collector-key: " + collector +
		"\n    service-key: " + reader + "\n    service-url: " + serviceURL + "\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, path, coreauth.NewManager(nil, nil, nil))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v8/management/config", h.ConfigV8)
	router.PUT("/v8/management/config/*path", h.ConfigV8)

	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v8/management/config", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
	for _, secret := range []string{collector, reader, serviceURL} {
		if strings.Contains(get.Body.String(), secret) {
			t.Fatal("v8 config read exposed a private allowance value")
		}
	}

	// A JSON write that replaces routing without the allowance block keeps the stored secrets.
	put := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v8/management/config/routing", strings.NewReader(`{"strategy":"fill-first"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(put, req)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{collector, reader, serviceURL} {
		if !strings.Contains(string(saved), secret) {
			t.Fatal("JSON write dropped a stored allowance value")
		}
	}
}
