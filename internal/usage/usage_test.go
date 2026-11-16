package usage

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// adversarialKeys are client keys whose text looks like a route, a fingerprint, an email,
// or contains whitespace. Typed storage must never persist any of them verbatim.
var adversarialKeys = []string{
	"/v1/chat/completions",
	"GET /v1/models",
	"user@example.com",
	"key:sha256:0123456789abcdef",
	"unattributed",
	"sk-plain-client-key-0001",
}

func encodedFor(t *testing.T, stats *RequestStatistics) []byte {
	t.Helper()
	data, err := EncodeSnapshot(stats.Snapshot())
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	return data
}

// TestRecordNeverStoresRawClientKeys checks every adversarial client key, whatever its shape.
func TestRecordNeverStoresRawClientKeys(t *testing.T) {
	stats := NewRequestStatistics()
	at := time.Now()
	for i, key := range adversarialKeys {
		stats.Record(context.Background(), coreusage.Record{
			RequestID: "req-" + string(rune('a'+i)), APIKey: key, Model: "m", RequestedAt: at,
			Source: key, AuthType: "apikey",
		})
	}
	data := encodedFor(t, stats)
	for _, key := range adversarialKeys {
		if bytes.Contains(data, []byte(key)) {
			t.Fatalf("snapshot contains raw client key %q: %s", key, data)
		}
	}
	if got := len(stats.Snapshot().APIs); got != len(adversarialKeys) {
		t.Fatalf("api buckets = %d, want %d", got, len(adversarialKeys))
	}
}

// TestSourceKeepsOnlyOAuthIdentity checks that account emails survive for OAuth records
// while API key sources, including ones containing "@", are fingerprinted.
func TestSourceKeepsOnlyOAuthIdentity(t *testing.T) {
	stats := NewRequestStatistics()
	at := time.Now()
	stats.Record(context.Background(), coreusage.Record{RequestID: "oauth-1", Model: "m", RequestedAt: at, AuthType: "oauth", Source: "account@example.com"})
	stats.Record(context.Background(), coreusage.Record{RequestID: "key-1", Model: "m", RequestedAt: at, AuthType: "apikey", Source: "upstream@key.example"})

	models := stats.Snapshot().APIs[unattributedName].Models["m"]
	bySource := map[string]string{}
	for _, d := range models.Details {
		bySource[d.RequestID] = d.Source
	}
	if bySource["oauth-1"] != "account@example.com" {
		t.Fatalf("oauth source = %q, want the account identity", bySource["oauth-1"])
	}
	if bySource["key-1"] != KeyFingerprint("upstream@key.example") {
		t.Fatalf("api key source = %q, want its fingerprint", bySource["key-1"])
	}
	if bytes.Contains(encodedFor(t, stats), []byte("upstream@key.example")) {
		t.Fatal("snapshot contains a raw upstream key source")
	}
}

// TestRecordIgnoresRequestContext checks that the plugin path reads only the record. The
// Gin context on the ctx is set to a 500 status and a route, neither of which may be used.
func TestRecordIgnoresRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rr := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rr)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/unmatched/sk-looks-like-a-key", nil)
	ginCtx.Writer.WriteHeader(http.StatusInternalServerError)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	stats := NewRequestStatistics()
	stats.Record(ctx, coreusage.Record{RequestID: "ctx-1", Model: "m", RequestedAt: time.Now()})

	snapshot := stats.Snapshot()
	if snapshot.SuccessCount != 1 || snapshot.FailureCount != 0 {
		t.Fatalf("context status changed the outcome: success=%d failure=%d", snapshot.SuccessCount, snapshot.FailureCount)
	}
	if _, ok := snapshot.APIs[unattributedName]; !ok {
		t.Fatalf("request without a client key is not grouped as unattributed: %v", snapshot.APIs)
	}
	if bytes.Contains(encodedFor(t, stats), []byte("sk-looks-like-a-key")) {
		t.Fatal("route text from the request context was stored")
	}
}

func TestRecordGeneratesRequestIDWhenMissing(t *testing.T) {
	stats := NewRequestStatistics()
	at := time.Now()
	stats.Record(context.Background(), coreusage.Record{Model: "m", RequestedAt: at})
	stats.Record(context.Background(), coreusage.Record{Model: "m", RequestedAt: at})
	ids := map[string]bool{}
	for _, d := range stats.Snapshot().APIs[unattributedName].Models["m"].Details {
		if d.RequestID == "" {
			t.Fatal("detail has an empty request_id")
		}
		ids[d.RequestID] = true
	}
	if len(ids) != 2 {
		t.Fatalf("identical records with no request ID collapsed to %d ids", len(ids))
	}
}

func TestPluginRespectsUsageStatisticsSwitch(t *testing.T) {
	prev := redisqueue.UsageStatisticsEnabled()
	defer redisqueue.SetUsageStatisticsEnabled(prev)

	stats := NewRequestStatistics()
	plugin := &usagePlugin{stats: stats}
	record := coreusage.Record{APIKey: "client-key", Model: "m", RequestedAt: time.Now()}

	redisqueue.SetUsageStatisticsEnabled(false)
	stats.SetCollecting(true)
	plugin.HandleUsage(context.Background(), record)
	if got := stats.Snapshot().TotalRequests; got != 0 {
		t.Fatalf("disabled usage statistics recorded %d requests", got)
	}

	redisqueue.SetUsageStatisticsEnabled(true)
	stats.SetCollecting(false)
	plugin.HandleUsage(context.Background(), record)
	if got := stats.Snapshot().TotalRequests; got != 0 {
		t.Fatalf("plugin recorded %d requests while collection is off", got)
	}

	stats.SetCollecting(true)
	plugin.HandleUsage(context.Background(), record)
	if got := stats.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("enabled plugin recorded %d requests, want 1", got)
	}
}

// TestSetCollectingFencesRecords checks that no record is accepted once collection is off.
func TestSetCollectingFencesRecords(t *testing.T) {
	stats := NewRequestStatistics()
	stats.SetCollecting(true)
	stats.RecordCollected(context.Background(), coreusage.Record{Model: "m", RequestedAt: time.Now()})
	stats.SetCollecting(false)
	stats.RecordCollected(context.Background(), coreusage.Record{Model: "m", RequestedAt: time.Now()})
	if got := stats.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("records after SetCollecting(false) = %d total, want 1", got)
	}
}

func TestResetClearsAggregateAndLoadedPaths(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{Model: "m", RequestedAt: time.Now()})
	stats.loadedPaths["/some/path"] = struct{}{}
	stats.Reset()
	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 0 || len(snapshot.APIs) != 0 || len(stats.loadedPaths) != 0 {
		t.Fatalf("Reset left state behind: %+v loaded=%v", snapshot, stats.loadedPaths)
	}
}

func TestPruneDropsExpiredDetailsAndKeepsTotals(t *testing.T) {
	stats := NewRequestStatistics()
	old := time.Now().Add(-(detailRetention + time.Hour))
	stats.Record(context.Background(), coreusage.Record{Model: "m", RequestedAt: old, Detail: coreusage.Detail{TotalTokens: 7}})
	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 1 || snapshot.TotalTokens != 7 {
		t.Fatalf("totals after prune = %d requests, %d tokens", snapshot.TotalRequests, snapshot.TotalTokens)
	}
	if got := len(snapshot.APIs[unattributedName].Models["m"].Details); got != 0 {
		t.Fatalf("expired details kept: %d", got)
	}
}
