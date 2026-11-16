package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

const handlerTestRawKey = "sk-example-handler-raw-0001"

func TestImportUsageSnapshotV8NormalizesRawKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"version":1,"usage":{"total_requests":1,"success_count":1,"failure_count":0,"total_tokens":4,
	  "apis":{"` + handlerTestRawKey + `":{"total_requests":1,"total_tokens":4,"models":{"m":{"total_requests":1,"total_tokens":4,
	  "details":[{"timestamp":"2026-10-01T10:00:00Z","source":"` + handlerTestRawKey + `","auth_index":"x","tokens":{"total_tokens":4},"failed":false}]}}}}}}`

	h := &Handler{}
	importRR := httptest.NewRecorder()
	importCtx, _ := gin.CreateTestContext(importRR)
	importCtx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/observability/usage/import", strings.NewReader(body))
	h.ImportUsageSnapshotV8(importCtx)
	if importRR.Code != http.StatusOK {
		t.Fatalf("import status = %d body=%s", importRR.Code, importRR.Body.String())
	}

	exportRR := httptest.NewRecorder()
	exportCtx, _ := gin.CreateTestContext(exportRR)
	exportCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/export", nil)
	h.ExportUsageSnapshotV8(exportCtx)
	if exportRR.Code != http.StatusOK {
		t.Fatalf("export status = %d", exportRR.Code)
	}
	if bytes.Contains(exportRR.Body.Bytes(), []byte(handlerTestRawKey)) {
		t.Fatal("export contains a raw API key")
	}
	if !bytes.Contains(exportRR.Body.Bytes(), []byte(usage.KeyFingerprint(handlerTestRawKey))) {
		t.Fatal("export lacks the key fingerprint")
	}
}

func TestImportUsageSnapshotV8RejectsUnsupportedVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/observability/usage/import", strings.NewReader(`{"version":9,"usage":{}}`))
	(&Handler{}).ImportUsageSnapshotV8(ctx)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
