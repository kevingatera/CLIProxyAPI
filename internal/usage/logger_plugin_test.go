package usage

import (
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestRecordIncludesAuthIDAndProvider(t *testing.T) {
	stats := NewRequestStatistics()
	when := time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC)

	stats.Record(nil, coreusage.Record{
		Provider:    "cursor",
		Model:       "cursor/auto",
		AuthID:      "cursor-1.json",
		AuthIndex:   "idx-cursor-1",
		RequestedAt: when,
		Failed:      true,
	})

	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 1 {
		t.Fatalf("snapshot.TotalRequests = %d, want 1", snapshot.TotalRequests)
	}
	if snapshot.FailureCount != 1 {
		t.Fatalf("snapshot.FailureCount = %d, want 1", snapshot.FailureCount)
	}

	apiSnapshot, ok := snapshot.APIs["cursor"]
	if !ok {
		t.Fatalf("snapshot.APIs missing key %q", "cursor")
	}
	modelSnapshot, ok := apiSnapshot.Models["cursor/auto"]
	if !ok {
		t.Fatalf("apiSnapshot.Models missing key %q", "cursor/auto")
	}
	if len(modelSnapshot.Details) != 1 {
		t.Fatalf("len(modelSnapshot.Details) = %d, want 1", len(modelSnapshot.Details))
	}

	detail := modelSnapshot.Details[0]
	if detail.AuthID != "cursor-1.json" {
		t.Fatalf("detail.AuthID = %q, want %q", detail.AuthID, "cursor-1.json")
	}
	if detail.Provider != "cursor" {
		t.Fatalf("detail.Provider = %q, want %q", detail.Provider, "cursor")
	}
	if detail.AuthIndex != "idx-cursor-1" {
		t.Fatalf("detail.AuthIndex = %q, want %q", detail.AuthIndex, "idx-cursor-1")
	}
}

func TestDedupKeyIncludesAuthIDAndProvider(t *testing.T) {
	baseDetail := RequestDetail{
		Timestamp: time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC),
		Source:    "src",
		AuthIndex: "idx",
		Tokens: TokenStats{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	}

	withAuthID := baseDetail
	withAuthID.AuthID = "cursor-a.json"
	withAuthID.Provider = "cursor"

	withDifferentAuthID := baseDetail
	withDifferentAuthID.AuthID = "cursor-b.json"
	withDifferentAuthID.Provider = "cursor"

	withDifferentProvider := baseDetail
	withDifferentProvider.AuthID = "cursor-a.json"
	withDifferentProvider.Provider = "openai"

	k1 := dedupKey("api", "model", withAuthID)
	k2 := dedupKey("api", "model", withDifferentAuthID)
	k3 := dedupKey("api", "model", withDifferentProvider)

	if k1 == k2 {
		t.Fatal("expected different auth_id values to produce distinct dedup keys")
	}
	if k1 == k3 {
		t.Fatal("expected different provider values to produce distinct dedup keys")
	}
}
