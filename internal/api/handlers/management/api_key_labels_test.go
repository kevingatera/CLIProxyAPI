package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
)

const apiKeyLabelsTestPath = "/v8/management/api-key-labels"

func setupAPIKeyLabelsHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configDir := t.TempDir()
	return &Handler{configFilePath: filepath.Join(configDir, "config.yaml")}, configDir
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

	w = doAPIKeyLabels(router, http.MethodPut, `{"api-key-labels":{"key:sha256:0123456789abcdef":"Build bot","key:sha256:fedcba9876543210":"Team laptop"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeAPIKeyLabels(t, doAPIKeyLabels(router, http.MethodGet, ""))
	if got["key:sha256:0123456789abcdef"] != "Build bot" || got["key:sha256:fedcba9876543210"] != "Team laptop" {
		t.Fatalf("labels = %v", got)
	}
}

func TestAPIKeyLabelsDropsEmptyEntriesAndAcceptsBareMap(t *testing.T) {
	h, _ := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	w := doAPIKeyLabels(router, http.MethodPut, `{"":"dropped","valid":"Kept","blank":"   "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", w.Code)
	}
	got := decodeAPIKeyLabels(t, w)
	if _, ok := got[""]; ok || got["valid"] != "Kept" || len(got) != 1 {
		t.Fatalf("labels = %v", got)
	}
}

func TestAPIKeyLabelsRejectsInvalidBody(t *testing.T) {
	h, _ := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	if w := doAPIKeyLabels(router, http.MethodPut, `["not","a","map"]`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400", w.Code)
	}
}

func TestAPIKeyLabelsRejectsStaleIfMatch(t *testing.T) {
	h, _ := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	first := doAPIKeyLabels(router, http.MethodGet, "").Header().Get("ETag")
	if w := doAPIKeyLabels(router, http.MethodPut, `{"a":"One"}`, first); w.Code != http.StatusOK {
		t.Fatalf("matching If-Match status = %d, want 200", w.Code)
	}

	// A second client still holds the first revision.
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

func TestAPIKeyLabelsFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file mode bits are not enforced on Windows")
	}
	h, configDir := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	if w := doAPIKeyLabels(router, http.MethodPut, `{"a":"One"}`); w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", w.Code)
	}
	info, err := os.Stat(filepath.Join(configDir, apiKeyLabelsFileName))
	if err != nil {
		t.Fatalf("labels file not written: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("labels file mode = %o, want 600", mode)
	}
}
