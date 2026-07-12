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
)

func setupAPIKeyLabelsHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	// ResolveLogDirectory falls back to a cwd-relative "logs" dir when cfg is
	// nil; clean that up so the test does not leak artifacts into the repo.
	t.Cleanup(func() {
		_ = os.RemoveAll("logs")
	})
	return &Handler{
		configFilePath: filepath.Join(dir, "config.yaml"),
	}
}

func newAPIKeyLabelsRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/v0/management/api-key-labels", h.GetAPIKeyLabels)
	r.PUT("/v0/management/api-key-labels", h.PutAPIKeyLabels)
	return r
}

func TestAPIKeyLabelsRoundTrip(t *testing.T) {
	h := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	// Initial get returns empty map (not 404).
	req := httptest.NewRequest(http.MethodGet, "/v0/management/api-key-labels", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("initial GET status = %d, want 200", w.Code)
	}
	var initial struct {
		Labels map[string]string `json:"api-key-labels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &initial); err != nil {
		t.Fatalf("unmarshal initial: %v", err)
	}
	if len(initial.Labels) != 0 {
		t.Fatalf("initial labels = %v, want empty", initial.Labels)
	}

	// Put some labels.
	body := []byte(`{"api-key-labels":{"abc123":"Cline","def456":"Cursor"}}`)
	req = httptest.NewRequest(http.MethodPut, "/v0/management/api-key-labels", bytes.NewReader(body))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Get returns the persisted labels.
	req = httptest.NewRequest(http.MethodGet, "/v0/management/api-key-labels", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("second GET status = %d, want 200", w.Code)
	}
	var after struct {
		Labels map[string]string `json:"api-key-labels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}
	if got := after.Labels["abc123"]; got != "Cline" {
		t.Fatalf("label abc123 = %q, want %q", got, "Cline")
	}
	if got := after.Labels["def456"]; got != "Cursor" {
		t.Fatalf("label def456 = %q, want %q", got, "Cursor")
	}
}

func TestAPIKeyLabelsDropsEmptyFingerprint(t *testing.T) {
	h := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	body := []byte(`{"api-key-labels":{"":"dropped","valid":"Kept"}}`)
	req := httptest.NewRequest(http.MethodPut, "/v0/management/api-key-labels", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT status = %d, want 200", w.Code)
	}

	var after struct {
		Labels map[string]string `json:"api-key-labels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, exists := after.Labels[""]; exists {
		t.Fatalf("empty-fingerprint label should have been dropped")
	}
	if got := after.Labels["valid"]; got != "Kept" {
		t.Fatalf("label valid = %q, want %q", got, "Kept")
	}
}

func TestAPIKeyLabelsAcceptsBareMap(t *testing.T) {
	h := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	// Bare map without the envelope wrapper.
	body := []byte(`{"abc123":"Cline"}`)
	req := httptest.NewRequest(http.MethodPut, "/v0/management/api-key-labels", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/api-key-labels", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var after struct {
		Labels map[string]string `json:"api-key-labels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := after.Labels["abc123"]; got != "Cline" {
		t.Fatalf("label abc123 = %q, want %q", got, "Cline")
	}
}
