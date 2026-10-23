package usage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	testClientKey   = "sk-example-client-key-0001"
	testUpstreamKey = "sk-example-upstream-key-0002"
)

func TestRecordStoresFingerprintNotRawKeys(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      testClientKey,
		Model:       "gpt-test",
		Source:      testUpstreamKey,
		Provider:    "openai",
		RequestedAt: time.Now(),
		Detail:      coreusage.Detail{InputTokens: 3, OutputTokens: 4},
	})

	snapshot := stats.Snapshot()
	if _, ok := snapshot.APIs[KeyFingerprint(testClientKey)]; !ok {
		t.Fatalf("expected fingerprint key, got APIs %v", keysOf(snapshot.APIs))
	}
	data, err := EncodeSnapshot(snapshot)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	for _, raw := range []string{testClientKey, testUpstreamKey} {
		if strings.Contains(string(data), raw) {
			t.Fatalf("snapshot contains raw key %q", raw)
		}
	}
	if !strings.Contains(string(data), KeyFingerprint(testUpstreamKey)) {
		t.Fatal("upstream key source should be stored as a fingerprint")
	}
}

func TestNormalizeAPINameAndSource(t *testing.T) {
	cases := []struct{ in, want string }{
		{"POST /v1/responses", "POST /v1/responses"},
		{"/v1/models", "/v1/models"},
		{"unknown", "unknown"},
		{"", "unknown"},
		{KeyFingerprint(testClientKey), KeyFingerprint(testClientKey)},
	}
	for _, tc := range cases {
		if got := normalizeAPIName(tc.in); got != tc.want {
			t.Errorf("normalizeAPIName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := normalizeAPIName(testClientKey); got != KeyFingerprint(testClientKey) {
		t.Errorf("raw key not fingerprinted: %q", got)
	}
	if got := normalizeSource("user@example.com"); got != "user@example.com" {
		t.Errorf("email source changed: %q", got)
	}
	if got := normalizeSource(testUpstreamKey); got != KeyFingerprint(testUpstreamKey) {
		t.Errorf("key source not fingerprinted: %q", got)
	}
}

func TestPluginRespectsUsageStatisticsSwitch(t *testing.T) {
	prev := redisqueue.UsageStatisticsEnabled()
	redisqueue.SetUsageStatisticsEnabled(false)
	defer redisqueue.SetUsageStatisticsEnabled(prev)

	stats := NewRequestStatistics()
	plugin := &usagePlugin{stats: stats}
	plugin.HandleUsage(context.Background(), coreusage.Record{APIKey: testClientKey, Model: "m", RequestedAt: time.Now()})
	if got := stats.Snapshot().TotalRequests; got != 0 {
		t.Fatalf("disabled plugin recorded %d requests", got)
	}

	redisqueue.SetUsageStatisticsEnabled(true)
	plugin.HandleUsage(context.Background(), coreusage.Record{APIKey: testClientKey, Model: "m", RequestedAt: time.Now()})
	if got := stats.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("enabled plugin recorded %d requests, want 1", got)
	}
}

func TestPruneDropsExpiredDetailsAndKeepsTotals(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Now()
	stats.Record(context.Background(), coreusage.Record{APIKey: testClientKey, Model: "m", RequestedAt: now.Add(-8 * 24 * time.Hour), Detail: coreusage.Detail{TotalTokens: 10}})
	stats.Record(context.Background(), coreusage.Record{APIKey: testClientKey, Model: "m", RequestedAt: now, Detail: coreusage.Detail{TotalTokens: 5}})

	snapshot := stats.Snapshot()
	model := snapshot.APIs[KeyFingerprint(testClientKey)].Models["m"]
	if len(model.Details) != 1 {
		t.Fatalf("details after prune = %d, want 1", len(model.Details))
	}
	if snapshot.TotalRequests != 2 || snapshot.TotalTokens != 15 {
		t.Fatalf("totals changed by prune: requests=%d tokens=%d", snapshot.TotalRequests, snapshot.TotalTokens)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
