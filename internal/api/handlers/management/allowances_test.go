package management

import (
	"net/http"
	"net/http/httptest"
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
