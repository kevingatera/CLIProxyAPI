package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
)

const (
	apiKeyLabelsTestPath = "/v8/management/api-key-labels"
	fpA                  = "key:sha256:0123456789abcdef"
	fpB                  = "key:sha256:fedcba9876543210"
)

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

	body := `{"api-key-labels":{"` + fpA + `":"Build bot","` + fpB + `":"Team laptop"}}`
	w = doAPIKeyLabels(router, http.MethodPut, body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := decodeAPIKeyLabels(t, doAPIKeyLabels(router, http.MethodGet, ""))
	if got[fpA] != "Build bot" || got[fpB] != "Team laptop" {
		t.Fatalf("labels = %v", got)
	}
}

func TestAPIKeyLabelsDropsBlankLabelsAndAcceptsBareMap(t *testing.T) {
	h, _ := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	w := doAPIKeyLabels(router, http.MethodPut, `{"`+fpA+`":"Kept","`+fpB+`":"   "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", w.Code)
	}
	got := decodeAPIKeyLabels(t, w)
	if got[fpA] != "Kept" || len(got) != 1 {
		t.Fatalf("labels = %v", got)
	}
}

// Fingerprints outside the documented syntax are rejected, including raw keys.
// The response must not echo the rejected value.
func TestAPIKeyLabelsRejectsInvalidFingerprints(t *testing.T) {
	h, configDir := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	for _, bad := range []string{
		"",
		"  ",
		"sk-live-raw-secret-123456",
		"key:sha256:ABCDEF0123456789",
		"key:sha256:0123",
		"key:sha256:0123456789abcdef0",
		"KEY:sha256:0123456789abcdef",
	} {
		w := doAPIKeyLabels(router, http.MethodPut, `{"`+bad+`":"Label"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT with fingerprint %q status = %d, want 400", bad, w.Code)
		}
		if strings.TrimSpace(bad) != "" && strings.Contains(w.Body.String(), bad) {
			t.Fatalf("400 response echoed the rejected value %q: %s", bad, w.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(configDir, apiKeyLabelsFileName)); !os.IsNotExist(err) {
		t.Fatalf("rejected writes created the labels file (err=%v)", err)
	}
}

// Empty, null and non-object bodies must not replace stored labels.
func TestAPIKeyLabelsRejectsEmptyAndNullBodiesWithoutLoss(t *testing.T) {
	h, _ := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)
	if w := doAPIKeyLabels(router, http.MethodPut, `{"`+fpA+`":"Keep me"}`); w.Code != http.StatusOK {
		t.Fatalf("seed PUT status = %d", w.Code)
	}

	for _, body := range []string{"", "   \n", "null", `{"api-key-labels":null}`, `{"api-key-labels":"x"}`, `"text"`, `[]`} {
		if w := doAPIKeyLabels(router, http.MethodPut, body); w.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q status = %d, want 400", body, w.Code)
		}
	}
	if got := decodeAPIKeyLabels(t, doAPIKeyLabels(router, http.MethodGet, "")); got[fpA] != "Keep me" || len(got) != 1 {
		t.Fatalf("rejected bodies changed labels: %v", got)
	}
}

// A null, empty or corrupt labels file is an error. It is not an empty label set,
// and a write must not overwrite it.
func TestAPIKeyLabelsRejectsInvalidPersistedFile(t *testing.T) {
	h, configDir := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)
	path := filepath.Join(configDir, apiKeyLabelsFileName)

	for _, content := range []string{"null", "", `{"api-key-labels":null}`, `{"sk-raw":"x"}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if w := doAPIKeyLabels(router, http.MethodGet, ""); w.Code != http.StatusInternalServerError {
			t.Fatalf("GET with file %q status = %d, want 500", content, w.Code)
		}
		if w := doAPIKeyLabels(router, http.MethodPut, `{"`+fpB+`":"New"}`); w.Code != http.StatusInternalServerError {
			t.Fatalf("PUT with file %q status = %d, want 500", content, w.Code)
		}
		if data, _ := os.ReadFile(path); string(data) != content {
			t.Fatalf("file %q was overwritten with %q", content, data)
		}
	}
}

// A failed rename must not leave the temp file behind. A directory at the target
// path makes the rename fail while the temp file is already written.
func TestAPIKeyLabelsRemovesTempFileOnFailedRename(t *testing.T) {
	h, configDir := setupAPIKeyLabelsHandler(t)
	target := filepath.Join(configDir, apiKeyLabelsFileName)
	if err := os.MkdirAll(filepath.Join(target, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := h.saveAPIKeyLabelsLocked(map[string]string{fpA: "Label"}); err == nil {
		t.Fatal("save succeeded, want rename failure")
	}
	leftovers, err := filepath.Glob(filepath.Join(configDir, "api-key-labels-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind after failed rename: %v", leftovers)
	}
}

func TestAPIKeyLabelsRejectsStaleIfMatch(t *testing.T) {
	h, _ := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	first := doAPIKeyLabels(router, http.MethodGet, "").Header().Get("ETag")
	if w := doAPIKeyLabels(router, http.MethodPut, `{"`+fpA+`":"One"}`, first); w.Code != http.StatusOK {
		t.Fatalf("matching If-Match status = %d, want 200", w.Code)
	}

	// A second client still holds the first revision.
	stale := doAPIKeyLabels(router, http.MethodPut, `{"`+fpB+`":"Two"}`, first)
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match status = %d, want 412", stale.Code)
	}
	current := stale.Header().Get("ETag")
	if current == "" || current == first {
		t.Fatalf("412 ETag = %q, want current revision", current)
	}
	if got := decodeAPIKeyLabels(t, doAPIKeyLabels(router, http.MethodGet, "")); got[fpB] != "" || got[fpA] != "One" {
		t.Fatalf("stale write changed labels: %v", got)
	}

	if w := doAPIKeyLabels(router, http.MethodPut, `{"`+fpB+`":"Two"}`, current); w.Code != http.StatusOK {
		t.Fatalf("current If-Match status = %d, want 200", w.Code)
	}
	if w := doAPIKeyLabels(router, http.MethodPut, `{"`+fpA+`":"Three"}`, "*"); w.Code != http.StatusOK {
		t.Fatalf("wildcard If-Match status = %d, want 200", w.Code)
	}
	if w := doAPIKeyLabels(router, http.MethodPut, `{"`+fpA+`":"Four"}`); w.Code != http.StatusOK {
		t.Fatalf("unconditional PUT status = %d, want 200", w.Code)
	}
}

func TestAPIKeyLabelsFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file mode bits are not enforced on Windows")
	}
	h, configDir := setupAPIKeyLabelsHandler(t)
	router := newAPIKeyLabelsRouter(h)

	if w := doAPIKeyLabels(router, http.MethodPut, `{"`+fpA+`":"One"}`); w.Code != http.StatusOK {
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

// Two handler instances on one labels path must not both win a conditional
// write. Each round starts from the same revision and sends two If-Match PUTs at
// once. Exactly one may succeed; the other must get 412.
func TestAPIKeyLabelsIndependentHandlersSerializeIfMatch(t *testing.T) {
	first, configDir := setupAPIKeyLabelsHandler(t)
	second := &Handler{configFilePath: filepath.Join(configDir, "config.yaml")}
	routerA := newAPIKeyLabelsRouter(first)
	routerB := newAPIKeyLabelsRouter(second)

	for round := 0; round < 100; round++ {
		etag := doAPIKeyLabels(routerA, http.MethodGet, "").Header().Get("ETag")
		start := make(chan struct{})
		codes := make(chan int, 2)
		var wg sync.WaitGroup
		for i, router := range []*gin.Engine{routerA, routerB} {
			wg.Add(1)
			// The ETag is content-based, so each write needs distinct content.
			// Otherwise an identical write would legitimately match the revision.
			body := `{"` + fpA + `":"writer-` + strconv.Itoa(round) + "-" + strconv.Itoa(i) + `"}`
			go func(router *gin.Engine, body string) {
				defer wg.Done()
				<-start
				codes <- doAPIKeyLabels(router, http.MethodPut, body, etag).Code
			}(router, body)
		}
		close(start)
		wg.Wait()
		close(codes)
		ok, precond := 0, 0
		for code := range codes {
			switch code {
			case http.StatusOK:
				ok++
			case http.StatusPreconditionFailed:
				precond++
			default:
				t.Fatalf("round %d: unexpected status %d", round, code)
			}
		}
		if ok != 1 || precond != 1 {
			t.Fatalf("round %d: %d writes succeeded and %d got 412, want exactly one of each", round, ok, precond)
		}
	}
}
