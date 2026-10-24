package usage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// legacyRawKeySnapshot is a synthetic version 1 file of the kind earlier builds
// wrote: raw client keys as apis map keys, and an upstream key in a detail's source.
const legacyRawKeySnapshot = `{
  "version": 1,
  "saved_at": "2026-10-01T00:00:00Z",
  "usage": {
    "total_requests": 3,
    "success_count": 2,
    "failure_count": 1,
    "total_tokens": 30,
    "apis": {
      "sk-example-legacy-client-0001": {
        "total_requests": 2,
        "total_tokens": 20,
        "models": {
          "gpt-test": {
            "total_requests": 2,
            "total_tokens": 20,
            "details": [
              {"timestamp": "2026-10-01T10:00:00Z", "source": "sk-example-legacy-upstream-0001", "auth_index": "a1", "tokens": {"total_tokens": 10}, "failed": false},
              {"timestamp": "2026-10-01T11:00:00Z", "source": "sk-example-legacy-upstream-0001", "auth_index": "a1", "tokens": {"total_tokens": 10}, "failed": true}
            ]
          }
        }
      },
      "POST /v1/responses": {
        "total_requests": 1,
        "total_tokens": 10,
        "models": {
          "gpt-test": {
            "total_requests": 1,
            "total_tokens": 10,
            "details": [
              {"timestamp": "2026-10-01T12:00:00Z", "source": "user@example.com", "auth_index": "a2", "tokens": {"total_tokens": 10}, "failed": false}
            ]
          }
        }
      }
    },
    "requests_by_day": {"2026-10-01": 3},
    "requests_by_hour": {"10": 1, "11": 1, "12": 1},
    "tokens_by_day": {"2026-10-01": 30},
    "tokens_by_hour": {"10": 10, "11": 10, "12": 10}
  }
}`

const (
	legacyClientKey   = "sk-example-legacy-client-0001"
	legacyUpstreamKey = "sk-example-legacy-upstream-0001"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadMigratesRawKeySnapshotOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_stats.json")
	writeTestFile(t, path, legacyRawKeySnapshot)

	stats := NewRequestStatistics()
	if err := NewPersistence(PersistenceConfig{Stats: stats, Path: path}).Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	for _, raw := range []string{legacyClientKey, legacyUpstreamKey} {
		if bytes.Contains(migrated, []byte(raw)) {
			t.Fatalf("migrated file still contains raw key %q", raw)
		}
	}
	if !bytes.Contains(migrated, []byte(KeyFingerprint(legacyClientKey))) {
		t.Fatal("migrated file lacks the client key fingerprint")
	}
	if !bytes.Contains(migrated, []byte(`"version":2`)) {
		t.Fatalf("migrated file is not version 2: %s", migrated)
	}

	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 3 || snapshot.FailureCount != 1 || snapshot.TotalTokens != 30 {
		t.Fatalf("totals lost in migration: %+v", snapshot)
	}
	if got := snapshot.APIs[KeyFingerprint(legacyClientKey)].TotalRequests; got != 2 {
		t.Fatalf("client key requests = %d, want 2", got)
	}
	if got := snapshot.APIs["POST /v1/responses"].TotalRequests; got != 1 {
		t.Fatalf("route requests = %d, want 1", got)
	}

	// A second load of the migrated file must not rewrite it.
	before := append([]byte(nil), migrated...)
	if err := NewPersistence(PersistenceConfig{Stats: NewRequestStatistics(), Path: path}).Load(); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("second load rewrote an already migrated file")
	}
}

func TestLoadMigratesBareLegacySnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_stats.json")
	bare := `{"total_requests":1,"success_count":1,"failure_count":0,"total_tokens":5,
	  "apis":{"sk-example-bare-0001":{"total_requests":1,"total_tokens":5,"models":{"m":{"total_requests":1,"total_tokens":5,"details":[]}}}},
	  "requests_by_day":{},"requests_by_hour":{},"tokens_by_day":{},"tokens_by_hour":{}}`
	writeTestFile(t, path, bare)

	stats := NewRequestStatistics()
	if err := NewPersistence(PersistenceConfig{Stats: stats, Path: path}).Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := stats.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("bare snapshot loaded %d requests, want 1", got)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "sk-example-bare-0001") || !strings.Contains(string(data), `"version":2`) {
		t.Fatalf("bare snapshot not migrated: %s", data)
	}
}

func TestLoadRejectsUnsupportedOrMalformedWithoutTouchingFile(t *testing.T) {
	cases := map[string]string{
		"future version": `{"version":9,"usage":{"total_requests":1}}`,
		"malformed":      `{not json`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage_stats.json")
			writeTestFile(t, path, content)
			err := NewPersistence(PersistenceConfig{Stats: NewRequestStatistics(), Path: path}).Load()
			if err == nil {
				t.Fatal("expected load error")
			}
			if got, _ := os.ReadFile(path); string(got) != content {
				t.Fatalf("file changed after rejected load: %s", got)
			}
		})
	}
}

func TestFlushFailedCommitKeepsLastGoodSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage_stats.json")
	stats := NewRequestStatistics()
	p := NewPersistence(PersistenceConfig{Stats: stats, Path: path})

	stats.Record(context.Background(), coreusage.Record{APIKey: legacyClientKey, Model: "m", RequestedAt: time.Now()})
	if err := p.Flush(); err != nil {
		t.Fatalf("first Flush: %v", err)
	}
	good, _ := os.ReadFile(path)

	prevRename := renameFile
	renameFile = func(string, string) error { return errors.New("simulated rename failure") }
	defer func() { renameFile = prevRename }()

	stats.Record(context.Background(), coreusage.Record{APIKey: legacyClientKey, Model: "m", RequestedAt: time.Now()})
	if err := p.Flush(); err == nil {
		t.Fatal("expected flush error")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(good, after) {
		t.Fatal("failed commit changed the last-good snapshot")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestExportImportRoundTripDeduplicates(t *testing.T) {
	src := NewRequestStatistics()
	src.Record(context.Background(), coreusage.Record{APIKey: legacyClientKey, Model: "m", RequestedAt: time.Now(), Detail: coreusage.Detail{TotalTokens: 7}})
	data, err := EncodeSnapshot(src.Snapshot())
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}

	snapshot, needsRewrite, err := DecodeSnapshot(data)
	if err != nil || needsRewrite {
		t.Fatalf("DecodeSnapshot current format: err=%v needsRewrite=%v", err, needsRewrite)
	}
	dst := NewRequestStatistics()
	if got, err := dst.MergeSnapshot(snapshot); err != nil || got.Added != 1 || got.Skipped != 0 {
		t.Fatalf("first merge = %+v, err %v", got, err)
	}
	if got, err := dst.MergeSnapshot(snapshot); err != nil || got.Added != 0 || got.Skipped != 1 {
		t.Fatalf("repeat merge = %+v, err %v", got, err)
	}
}

func TestImportOfRawKeyExportStoresFingerprints(t *testing.T) {
	snapshot, _, err := DecodeSnapshot([]byte(legacyRawKeySnapshot))
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	dst := NewRequestStatistics()
	if _, err := dst.MergeSnapshot(snapshot); err != nil {
		t.Fatalf("MergeSnapshot: %v", err)
	}
	data, err := EncodeSnapshot(dst.Snapshot())
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	if bytes.Contains(data, []byte(legacyClientKey)) || bytes.Contains(data, []byte(legacyUpstreamKey)) {
		t.Fatal("imported raw keys reached the stored snapshot")
	}
}

func TestLoadFailedCommitLeavesFileAndAggregateUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_stats.json")
	writeTestFile(t, path, legacyRawKeySnapshot)
	stats := NewRequestStatistics()
	p := NewPersistence(PersistenceConfig{Stats: stats, Path: path})

	prevRename := renameFile
	renameFile = func(string, string) error { return errors.New("simulated rename failure") }
	defer func() { renameFile = prevRename }()

	if err := p.Load(); err == nil {
		t.Fatal("expected migration commit error")
	}
	if got := stats.Snapshot().TotalRequests; got != 0 {
		t.Fatalf("failed load changed the aggregate: total = %d", got)
	}
	if got, _ := os.ReadFile(path); string(got) != legacyRawKeySnapshot {
		t.Fatal("failed load changed the file")
	}
	if err := p.Flush(); err == nil {
		t.Fatal("Flush must not overwrite a snapshot that failed to load")
	}
	if got, _ := os.ReadFile(path); string(got) != legacyRawKeySnapshot {
		t.Fatal("refused flush changed the file")
	}

	renameFile = prevRename
	if err := p.Load(); err != nil {
		t.Fatalf("retry Load: %v", err)
	}
	if got := stats.Snapshot().TotalRequests; got != 3 {
		t.Fatalf("retry total = %d, want 3", got)
	}
	if err := p.Load(); err != nil {
		t.Fatalf("repeat Load: %v", err)
	}
	if got := stats.Snapshot().TotalRequests; got != 3 {
		t.Fatalf("repeat Load double counted: total = %d, want 3", got)
	}
}

func TestImportRejectsPrunedTotalsIntoNonEmptyAggregate(t *testing.T) {
	snapshot, _, err := DecodeSnapshot([]byte(legacyRawKeySnapshot))
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	dst := NewRequestStatistics()
	if _, err := dst.MergeSnapshot(snapshot); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if got := dst.Snapshot().TotalRequests; got != 3 {
		t.Fatalf("first import total = %d, want 3", got)
	}
	if _, err := dst.MergeSnapshot(snapshot); !errors.Is(err, ErrOverlappingSnapshot) {
		t.Fatalf("second import err = %v, want ErrOverlappingSnapshot", err)
	}
	if got := dst.Snapshot().TotalRequests; got != 3 {
		t.Fatalf("rejected import changed totals: %d, want 3", got)
	}
}

func TestLoadSamePathTwiceMergesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_stats.json")
	writeTestFile(t, path, legacyRawKeySnapshot)
	stats := NewRequestStatistics()

	if err := NewPersistence(PersistenceConfig{Stats: stats, Path: path}).Load(); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	// A restarted Persistence for the same file sees pruned totals; it must not reject or re-add them.
	p := NewPersistence(PersistenceConfig{Stats: stats, Path: path})
	if err := p.Load(); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if got := stats.Snapshot().TotalRequests; got != 3 {
		t.Fatalf("total after reload = %d, want 3", got)
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush after reload: %v", err)
	}
}
