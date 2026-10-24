package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const usageStatsRouteRawKey = "sk-usage-route-raw-0001"

// usageStatsRouteResponse mirrors the fields the management UI reads from GET /usage.
type usageStatsRouteResponse struct {
	Usage struct {
		TotalRequests int64 `json:"total_requests"`
		APIs          map[string]struct {
			Models map[string]struct {
				TotalRequests int64 `json:"total_requests"`
				Details       []struct {
					Source    string `json:"source"`
					AuthIndex string `json:"auth_index"`
				} `json:"details"`
			} `json:"models"`
		} `json:"apis"`
	} `json:"usage"`
	FailedRequests int64 `json:"failed_requests"`
}

func TestGetUsageStatisticsServesFingerprintedSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "usage-route-fingerprint-model"
	usage.GetRequestStatistics().Record(context.Background(), coreusage.Record{
		Model:     model,
		APIKey:    usageStatsRouteRawKey,
		Source:    usageStatsRouteRawKey,
		AuthIndex: "usage-route-auth-index",
	})

	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage", nil)
	(&Handler{}).GetUsageStatistics(ctx)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(usageStatsRouteRawKey)) {
		t.Fatal("usage route response contains a raw API key")
	}

	var payload usageStatsRouteResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	if payload.Usage.TotalRequests < 1 {
		t.Fatalf("usage.total_requests = %d, want at least 1", payload.Usage.TotalRequests)
	}
	apiSnapshot, ok := payload.Usage.APIs[usage.KeyFingerprint(usageStatsRouteRawKey)]
	if !ok {
		t.Fatalf("usage.apis lacks the key fingerprint; keys=%v", keysOf(payload.Usage.APIs))
	}
	modelSnapshot, ok := apiSnapshot.Models[model]
	if !ok || modelSnapshot.TotalRequests < 1 {
		t.Fatalf("model %q missing from fingerprinted api snapshot", model)
	}
	found := false
	for _, detail := range modelSnapshot.Details {
		if detail.AuthIndex == "usage-route-auth-index" {
			found = true
			if detail.Source != usage.KeyFingerprint(usageStatsRouteRawKey) {
				t.Fatalf("detail source = %q, want fingerprint", detail.Source)
			}
		}
	}
	if !found {
		t.Fatal("recorded detail missing from the usage route response")
	}
}

func TestGetUsageStatisticsKeepsFailedRequestsField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage", nil)
	(&Handler{}).GetUsageStatistics(ctx)

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := payload["usage"]; !ok {
		t.Fatalf("response lacks the usage object: %s", rr.Body.String())
	}
	if _, ok := payload["failed_requests"]; !ok {
		t.Fatalf("response lacks failed_requests: %s", rr.Body.String())
	}
}

// TestImportUsageSnapshotV8AcceptsReleasedV0Export checks that an export written by the
// released build (raw keys, exported_at wrapper) is converted to fingerprints on import.
func TestImportUsageSnapshotV8AcceptsReleasedV0Export(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const rawKey = "sk-v0-export-raw-0001"
	// Recent details stay inside the retention window, so the import merges into a
	// non-empty aggregate. Older details are covered by the retention rule instead.
	recent := time.Now().UTC().Format(time.RFC3339)
	body := `{"version":1,"exported_at":"` + recent + `","usage":{"total_requests":1,"success_count":1,"failure_count":0,"total_tokens":2,
	  "apis":{"` + rawKey + `":{"total_requests":1,"total_tokens":2,"models":{"v0-model":{"total_requests":1,"total_tokens":2,
	  "details":[{"timestamp":"` + recent + `","source":"` + rawKey + `","auth_index":"v0","tokens":{"total_tokens":2},"failed":false}]}}}}}}`

	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/observability/usage/import", bytes.NewBufferString(body))
	(&Handler{}).ImportUsageSnapshotV8(ctx)
	if rr.Code != http.StatusOK {
		t.Fatalf("import status = %d body=%s", rr.Code, rr.Body.String())
	}

	exportRR := httptest.NewRecorder()
	exportCtx, _ := gin.CreateTestContext(exportRR)
	exportCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/export", nil)
	(&Handler{}).ExportUsageSnapshotV8(exportCtx)
	if bytes.Contains(exportRR.Body.Bytes(), []byte(rawKey)) {
		t.Fatal("export after importing a v0 export contains the raw key")
	}
	if !bytes.Contains(exportRR.Body.Bytes(), []byte(usage.KeyFingerprint(rawKey))) {
		t.Fatal("export after importing a v0 export lacks the fingerprint")
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
