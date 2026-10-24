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

const apiKeyLabelsTestPath = "/v8/management/api-key-labels"

// setupAPIKeyLabelsHandler points the stable store at a temp config directory
// and the legacy log directory at another temp directory.
func setupAPIKeyLabelsHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configDir := t.TempDir()
	return &Handler{
		configFilePath: filepath.Join(configDir, "config.yaml"),
		logDir:         t.TempDir(),
	}, configDir
}

func newAPIKeyLabelsRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET(apiKeyLabelsTestPath, h.GetAPIKeyLabels)
	r.PUT(apiKeyLabelsTestPath, h.PutAPIKeyLabels)
	return r
}

func doAPIKeyLabels(r *gin.Engine, method, body string, ifMatch ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, apiKeyLabelsTestPath, bytes.NewReader([]byte(body)))
	for _, v := range ifMatch {
		req.Header.Add("If-Match", v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeAPIKeyLabels(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var out struct {
		Labels map[string]string `json:"api-key-labels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	return out.Labels
}

func TestAPIKeyLabelsRoundTrip(t *testing.T) {
	h, _ := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	w := doAPIKeyLabels(router, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("initial GET status = %d, want 200", w.Code)
	}
	if got := decodeAPIKeyLabels(t, w); len(got) != 0 {
		t.Fatalf("initial labels = %v, want empty", got)
	}
	if w.Header().Get("ETag") == "" {
		t.Fatal("GET missing ETag")
	}

	w = doAPIKeyLabels(router, http.MethodPut, `{"api-key-labels":{"abc123":"Cline","def456":"Cursor"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeAPIKeyLabels(t, doAPIKeyLabels(router, http.MethodGet, ""))
	if got["abc123"] != "Cline" || got["def456"] != "Cursor" {
		t.Fatalf("labels = %v", got)
	}
}

func TestAPIKeyLabelsDropsEmptyFingerprintAndAcceptsBareMap(t *testing.T) {
	h, _ := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	w := doAPIKeyLabels(router, http.MethodPut, `{"":"dropped","valid":"Kept"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", w.Code)
	}
	got := decodeAPIKeyLabels(t, w)
	if _, ok := got[""]; ok || got["valid"] != "Kept" {
		t.Fatalf("labels = %v", got)
	}
}

func TestAPIKeyLabelsRejectsStaleIfMatch(t *testing.T) {
	h, _ := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	first := doAPIKeyLabels(router, http.MethodGet, "").Header().Get("ETag")
	if w := doAPIKeyLabels(router, http.MethodPut, `{"a":"One"}`, first); w.Code != http.StatusOK {
		t.Fatalf("matching If-Match status = %d, want 200", w.Code)
	}

	// Second tab still holds the first revision.
	stale := doAPIKeyLabels(router, http.MethodPut, `{"b":"Two"}`, first)
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match status = %d, want 412", stale.Code)
	}
	current := stale.Header().Get("ETag")
	if current == "" || current == first {
		t.Fatalf("412 ETag = %q, want current revision", current)
	}
	if got := decodeAPIKeyLabels(t, doAPIKeyLabels(router, http.MethodGet, "")); got["b"] != "" || got["a"] != "One" {
		t.Fatalf("stale write changed labels: %v", got)
	}

	if w := doAPIKeyLabels(router, http.MethodPut, `{"b":"Two"}`, current); w.Code != http.StatusOK {
		t.Fatalf("current If-Match status = %d, want 200", w.Code)
	}
	if w := doAPIKeyLabels(router, http.MethodPut, `{"c":"Three"}`, "*"); w.Code != http.StatusOK {
		t.Fatalf("wildcard If-Match status = %d, want 200", w.Code)
	}
	if w := doAPIKeyLabels(router, http.MethodPut, `{"d":"Four"}`); w.Code != http.StatusOK {
		t.Fatalf("unconditional PUT status = %d, want 200", w.Code)
	}
}

func TestAPIKeyLabelsStoreOutsideLogDirAndMigrates(t *testing.T) {
	h, configDir := setupAPIKeyLabelsHandler(t)
	legacy := filepath.Join(h.logDir, apiKeyLabelsFileName)
	if err := os.WriteFile(legacy, []byte(`{"api-key-labels":{"abc":"Legacy"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	router := newAPIKeyLabelsRouter(h)

	if got := decodeAPIKeyLabels(t, doAPIKeyLabels(router, http.MethodGet, "")); got["abc"] != "Legacy" {
		t.Fatalf("migrated labels = %v, want abc=Legacy", got)
	}
	stable := filepath.Join(configDir, apiKeyLabelsFileName)
	if _, err := os.Stat(stable); err != nil {
		t.Fatalf("stable store not written: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file should remain for rollback: %v", err)
	}

	// Once the stable file exists it wins, even if the legacy file changes.
	if err := os.WriteFile(legacy, []byte(`{"abc":"Stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := doAPIKeyLabels(router, http.MethodPut, `{"abc":"Current"}`); w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", w.Code)
	}
	if got := decodeAPIKeyLabels(t, doAPIKeyLabels(router, http.MethodGet, "")); got["abc"] != "Current" {
		t.Fatalf("labels = %v, want stable value", got)
	}
}
