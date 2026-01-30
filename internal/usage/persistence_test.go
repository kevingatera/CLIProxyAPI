package usage

import (
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestPersistenceFlushThenLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage_stats.json")

	stats1 := NewRequestStatistics()
	stats1.Record(nil, coreusage.Record{
		Model:       "test-model",
		APIKey:      "test-key",
		RequestedAt: time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
		},
	})

	p1 := NewPersistence(PersistenceConfig{
		Stats:    stats1,
		Path:     path,
		Interval: time.Hour,
	})
	if err := p1.Flush(); err != nil {
		t.Fatalf("Flush() error: %v", err)
	}

	stats2 := NewRequestStatistics()
	p2 := NewPersistence(PersistenceConfig{
		Stats:    stats2,
		Path:     path,
		Interval: time.Hour,
	})
	if err := p2.Load(); err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	snapshot := stats2.Snapshot()
	if snapshot.TotalRequests != 1 {
		t.Fatalf("unexpected TotalRequests: got %d want %d", snapshot.TotalRequests, 1)
	}
	if snapshot.TotalTokens != 30 {
		t.Fatalf("unexpected TotalTokens: got %d want %d", snapshot.TotalTokens, 30)
	}
}
