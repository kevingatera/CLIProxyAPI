package usage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// recordsFor builds a stats aggregate holding n requests with identical fields and distinct IDs.
func recordsFor(t *testing.T, n int, at time.Time) *RequestStatistics {
	t.Helper()
	stats := NewRequestStatistics()
	for i := 0; i < n; i++ {
		stats.Record(context.Background(), coreusage.Record{
			RequestID: "id-" + string(rune('A'+i)), APIKey: "client-key-" + string(rune('a'+i)), Model: "m",
			RequestedAt: at, AuthType: "apikey", Source: "upstream-key", Detail: coreusage.Detail{TotalTokens: 3},
		})
	}
	return stats
}

// TestImportDedupesByRequestIDAndKeepsIdenticalRecords checks that two records with the same
// timestamp and fields both survive, and that re-importing the same export adds nothing.
func TestImportDedupesByRequestIDAndKeepsIdenticalRecords(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	source := NewRequestStatistics()
	for i := 0; i < 2; i++ {
		source.Record(context.Background(), coreusage.Record{
			RequestID: []string{"same-1", "same-2"}[i], Model: "m", RequestedAt: at, Detail: coreusage.Detail{TotalTokens: 5},
		})
	}
	data := encodedFor(t, source)
	snapshot, err := DecodeSnapshot(data)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}

	target := NewRequestStatistics()
	result, err := target.MergeSnapshot(snapshot)
	if err != nil || result.Added != 2 {
		t.Fatalf("first import: result=%+v err=%v, want 2 added", result, err)
	}
	result, err = target.MergeSnapshot(snapshot)
	if err != nil || result.Added != 0 || result.Skipped != 2 {
		t.Fatalf("repeat import: result=%+v err=%v, want 2 skipped", result, err)
	}
	if got := target.Snapshot().TotalRequests; got != 2 {
		t.Fatalf("total after repeat import = %d, want 2", got)
	}
}

func TestExportImportRoundTripKeepsFingerprints(t *testing.T) {
	source := recordsFor(t, 2, time.Now())
	data := encodedFor(t, source)
	snapshot, err := DecodeSnapshot(data)
	if err != nil {
		t.Fatalf("DecodeSnapshot(export): %v", err)
	}
	target := NewRequestStatistics()
	if _, err := target.MergeSnapshot(snapshot); err != nil {
		t.Fatalf("MergeSnapshot: %v", err)
	}
	if got, want := target.Snapshot().TotalTokens, source.Snapshot().TotalTokens; got != want {
		t.Fatalf("tokens after round trip = %d, want %d", got, want)
	}
}

// TestDecodeRejectsSnapshotsWithoutRequiredStructure covers shapes that must never be loaded
// or rewritten: unrelated JSON, legacy and unsupported versions, missing members, broken
// invariants, and raw key names.
func TestDecodeRejectsSnapshotsWithoutRequiredStructure(t *testing.T) {
	valid := `{"version":2,"saved_at":"2026-11-15T00:00:00Z","usage":{"total_requests":0,"success_count":0,"failure_count":0,"total_tokens":0,"apis":{},"requests_by_day":{},"requests_by_hour":{},"tokens_by_day":{},"tokens_by_hour":{}}}`
	if _, err := DecodeSnapshot([]byte(valid)); err != nil {
		t.Fatalf("baseline valid snapshot rejected: %v", err)
	}
	cases := map[string]string{
		"empty object":       `{}`,
		"unrelated json":     `{"name":"notes","items":[1,2]}`,
		"no version":         `{"usage":{"total_requests":0}}`,
		"version 1":          `{"version":1,"usage":{"total_requests":0}}`,
		"bare legacy":        `{"total_requests":0,"success_count":0,"failure_count":0,"total_tokens":0,"apis":{}}`,
		"usage object empty": `{"version":2,"usage":{}}`,
		"usage null":         `{"version":2,"usage":null}`,
		"missing member": `{"version":2,"usage":{"total_requests":0,"success_count":0,"failure_count":0,"total_tokens":0,"apis":{},
			"requests_by_day":{},"requests_by_hour":{},"tokens_by_day":{}}}`,
		"counts do not sum": `{"version":2,"usage":{"total_requests":2,"success_count":0,"failure_count":0,"total_tokens":0,"apis":{},
			"requests_by_day":{},"requests_by_hour":{},"tokens_by_day":{},"tokens_by_hour":{}}}`,
		"raw key name": `{"version":2,"usage":{"total_requests":1,"success_count":1,"failure_count":0,"total_tokens":0,
			"apis":{"sk-raw-client-key":{"total_requests":1,"total_tokens":0,"models":{"m":{"total_requests":1,"total_tokens":0,
			"details":[{"request_id":"r1","timestamp":"2026-11-15T00:00:00Z","auth_type":"apikey","source":"","failed":false,"tokens":{}}]}}}},
			"requests_by_day":{"2026-11-15":1},"requests_by_hour":{"00":1},"tokens_by_day":{},"tokens_by_hour":{}}}`,
		"raw api key source": `{"version":2,"usage":{"total_requests":1,"success_count":1,"failure_count":0,"total_tokens":0,
			"apis":{"unattributed":{"total_requests":1,"total_tokens":0,"models":{"m":{"total_requests":1,"total_tokens":0,
			"details":[{"request_id":"r1","timestamp":"2026-11-15T00:00:00Z","auth_type":"apikey","source":"sk-raw-upstream","failed":false,"tokens":{}}]}}}},
			"requests_by_day":{"2026-11-15":1},"requests_by_hour":{"00":1},"tokens_by_day":{},"tokens_by_hour":{}}}`,
		"detail without request id": `{"version":2,"usage":{"total_requests":1,"success_count":1,"failure_count":0,"total_tokens":0,
			"apis":{"unattributed":{"total_requests":1,"total_tokens":0,"models":{"m":{"total_requests":1,"total_tokens":0,
			"details":[{"timestamp":"2026-11-15T00:00:00Z","auth_type":"oauth","source":"a@b.c","failed":false,"tokens":{}}]}}}},
			"requests_by_day":{"2026-11-15":1},"requests_by_hour":{"00":1},"tokens_by_day":{},"tokens_by_hour":{}}}`,
	}
	for name, body := range cases {
		if _, err := DecodeSnapshot([]byte(body)); err == nil {
			t.Errorf("%s: accepted, want rejection", name)
		}
	}
}

// TestLoadRejectedFileIsNeverRewritten checks that a file failing validation stays byte-identical,
// and that a blocked Persistence refuses to write over it.
func TestLoadRejectedFileIsNeverRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_stats.json")
	original := []byte(`{"name":"unrelated","items":[1,2,3]}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	stats := NewRequestStatistics()
	p := NewPersistence(PersistenceConfig{Stats: stats, Path: path, Interval: time.Hour})
	if err := p.Load(); err == nil {
		t.Fatal("Load accepted an unrelated file")
	}
	if err := p.Flush(); !errors.Is(err, errSnapshotNotLoaded) {
		t.Fatalf("Flush after rejected load = %v, want errSnapshotNotLoaded", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("rejected file was rewritten: %s", after)
	}
}

// TestFlushFailedCommitKeepsLastGoodSnapshot simulates a failed rename. The previous file must
// survive and no temp file may be left behind.
func TestFlushFailedCommitKeepsLastGoodSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage_stats.json")
	good := encodedFor(t, recordsFor(t, 1, time.Now()))
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	stats := NewRequestStatistics()
	p := NewPersistence(PersistenceConfig{Stats: stats, Path: path, Interval: time.Hour})
	if err := p.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	prev := renameFile
	renameFile = func(string, string) error { return errors.New("simulated commit failure") }
	defer func() { renameFile = prev }()

	stats.Record(context.Background(), coreusage.Record{RequestID: "later", Model: "m", RequestedAt: time.Now()})
	if err := p.Flush(); err == nil {
		t.Fatal("Flush succeeded with a failing rename")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, good) {
		t.Fatal("failed commit changed the last good snapshot")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %d entries", len(entries))
	}
}

// TestLoadSamePathTwiceMergesOnce checks that reloading the same file in one process does not
// count its requests twice.
func TestLoadSamePathTwiceMergesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_stats.json")
	if err := os.WriteFile(path, encodedFor(t, recordsFor(t, 2, time.Now())), 0o600); err != nil {
		t.Fatal(err)
	}
	stats := NewRequestStatistics()
	for i := 0; i < 2; i++ {
		p := NewPersistence(PersistenceConfig{Stats: stats, Path: path, Interval: time.Hour})
		if err := p.Load(); err != nil {
			t.Fatalf("Load %d: %v", i, err)
		}
	}
	if got := stats.Snapshot().TotalRequests; got != 2 {
		t.Fatalf("total after two loads of one path = %d, want 2", got)
	}
}

// TestStopWritesFinalSnapshotWithoutStart checks that Stop flushes even when periodic writes
// never started.
func TestStopWritesFinalSnapshotWithoutStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_stats.json")
	stats := recordsFor(t, 1, time.Now())
	p := NewPersistence(PersistenceConfig{Stats: stats, Path: path, Interval: time.Hour})
	p.Stop()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("final snapshot missing: %v", err)
	}
	if _, err := DecodeSnapshot(data); err != nil {
		t.Fatalf("final snapshot does not validate: %v", err)
	}
}
