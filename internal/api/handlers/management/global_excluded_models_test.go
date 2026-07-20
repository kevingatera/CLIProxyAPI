package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func setupGlobalExcludedHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	// Create an empty config file so persist() can write to it.
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("failed to create config file: %v", err)
	}
	cfg := &config.Config{}
	h := &Handler{
		cfg:            cfg,
		configFilePath: configPath,
	}
	return h, dir
}

func newGlobalExcludedRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/v0/management/global-excluded-models", h.GetGlobalExcludedModels)
	r.PUT("/v0/management/global-excluded-models", h.PutGlobalExcludedModels)
	r.PATCH("/v0/management/global-excluded-models", h.PatchGlobalExcludedModels)
	r.DELETE("/v0/management/global-excluded-models", h.DeleteGlobalExcludedModels)
	return r
}

func TestGlobalExcludedModelsEmptyByDefault(t *testing.T) {
	h, _ := setupGlobalExcludedHandler(t)
	router := newGlobalExcludedRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/global-excluded-models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("GET status = %d, want 200", w.Code)
	}
	var resp struct {
		Models []string `json:"global-excluded-models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Models) != 0 {
		t.Fatalf("expected empty list, got %v", resp.Models)
	}
}

func TestGlobalExcludedModelsPutNormalizes(t *testing.T) {
	h, _ := setupGlobalExcludedHandler(t)
	router := newGlobalExcludedRouter(h)

	// PUT with mixed-case + whitespace; should be normalized to lowercase, trimmed, deduped.
	body := []byte(`["Cursor/*", " *-mini ", "gpt-5-codex", "cursor/*"]`)
	req := httptest.NewRequest(http.MethodPut, "/v0/management/global-excluded-models", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Verify via GET
	req = httptest.NewRequest(http.MethodGet, "/v0/management/global-excluded-models", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var resp struct {
		Models []string `json:"global-excluded-models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Should be: cursor/*, *-mini, gpt-5-codex (deduped, lowercase, trimmed)
	if len(resp.Models) != 3 {
		t.Fatalf("expected 3 patterns after dedup, got %d: %v", len(resp.Models), resp.Models)
	}
	expected := map[string]bool{"cursor/*": true, "*-mini": true, "gpt-5-codex": true}
	for _, m := range resp.Models {
		if !expected[m] {
			t.Fatalf("unexpected pattern %q in %v", m, resp.Models)
		}
	}
}

func TestGlobalExcludedModelsPatchAddAndRemove(t *testing.T) {
	h, _ := setupGlobalExcludedHandler(t)
	router := newGlobalExcludedRouter(h)

	// Add a pattern
	body := []byte(`{"model": "cursor/*"}`)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/global-excluded-models", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PATCH add status = %d, want 200", w.Code)
	}

	if len(h.cfg.GlobalExcludedModels) != 1 || h.cfg.GlobalExcludedModels[0] != "cursor/*" {
		t.Fatalf("expected [cursor/*], got %v", h.cfg.GlobalExcludedModels)
	}

	// Remove it
	body = []byte(`{"model": "cursor/*", "remove": true}`)
	req = httptest.NewRequest(http.MethodPatch, "/v0/management/global-excluded-models", bytes.NewReader(body))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PATCH remove status = %d, want 200", w.Code)
	}

	if len(h.cfg.GlobalExcludedModels) != 0 {
		t.Fatalf("expected empty after remove, got %v", h.cfg.GlobalExcludedModels)
	}
}

func TestGlobalExcludedModelsDeleteByModel(t *testing.T) {
	h, _ := setupGlobalExcludedHandler(t)
	h.cfg.GlobalExcludedModels = []string{"cursor/*", "gpt-5-codex"}
	router := newGlobalExcludedRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/v0/management/global-excluded-models?model=cursor%2F*", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("DELETE status = %d, want 200", w.Code)
	}

	if len(h.cfg.GlobalExcludedModels) != 1 || h.cfg.GlobalExcludedModels[0] != "gpt-5-codex" {
		t.Fatalf("expected [gpt-5-codex] after deletion, got %v", h.cfg.GlobalExcludedModels)
	}
}
