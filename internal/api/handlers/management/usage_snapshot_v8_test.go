package management

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const handlerTestRawKey = "sk-example-handler-raw-0001"

func importBody(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/observability/usage/import", strings.NewReader(body))
	h.ImportUsageSnapshotV8(ctx)
	return rr
}

// TestImportUsageSnapshotV8RejectsRawKeys checks that an import carrying a raw key is refused,
// so it can never reach the aggregate or a later export.
func TestImportUsageSnapshotV8RejectsRawKeys(t *testing.T) {
	body := `{"version":2,"usage":{"total_requests":1,"success_count":1,"failure_count":0,"total_tokens":4,
	  "apis":{"` + handlerTestRawKey + `":{"total_requests":1,"total_tokens":4,"models":{"m":{"total_requests":1,"total_tokens":4,
	  "details":[{"request_id":"raw-1","timestamp":"2026-10-01T10:00:00Z","auth_type":"apikey","source":"` + handlerTestRawKey + `","auth_index":"x","tokens":{"total_tokens":4},"failed":false}]}}}},
	  "requests_by_day":{"2026-10-01":1},"requests_by_hour":{"10":1},"tokens_by_day":{"2026-10-01":4},"tokens_by_hour":{"10":4}}}`
	rr := importBody(t, &Handler{}, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("raw-key import status = %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(handlerTestRawKey)) {
		t.Fatal("error response echoes the raw key")
	}
}

// TestUsageSnapshotV8ExportImportRoundTrip checks that an export imports into an aggregate
// and that the export holds no raw keys.
func TestUsageSnapshotV8ExportImportRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.GetRequestStatistics().Record(context.Background(), coreusage.Record{
		RequestID: "handler-roundtrip-1", APIKey: handlerTestRawKey, Model: "m", RequestedAt: time.Now(),
	})

	h := &Handler{}
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

	if rr := importBody(t, h, exportRR.Body.String()); rr.Code != http.StatusOK {
		t.Fatalf("re-import of own export status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestImportUsageSnapshotV8RejectsUnsupportedVersion(t *testing.T) {
	if rr := importBody(t, &Handler{}, `{"version":9,"usage":{}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if rr := importBody(t, &Handler{}, `{"version":1,"usage":{"total_requests":0}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("version 1 status = %d, want 400", rr.Code)
	}
}
