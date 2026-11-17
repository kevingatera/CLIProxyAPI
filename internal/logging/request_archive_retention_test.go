package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRequestArchiveConcurrentWritesDoNotWaitForRetention(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxSizeGB: 30})
	if a.startupErr != nil {
		t.Fatal(a.startupErr)
	}
	dir := a.file.logsDir
	const existing, writers = 3000, 32
	for i := range existing {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%sold-%d.log", archiveFilePrefix, i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.retain(); err != nil {
		t.Fatal(err)
	}
	if got := a.totalBytes.Load(); got != existing {
		t.Fatalf("total after sweep %d, want %d", got, existing)
	}
	lastSweep := a.lastSweep.Load()

	// Hold the retention lock as a running sweep would. Writers that needed it,
	// or that queued behind each other, could not finish.
	a.retentionMu.Lock()
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- a.LogRequest("/v1/models", "GET", nil, nil, 401, nil, []byte("denied"), nil, nil, nil, nil, nil, fmt.Sprintf("id%d", i), time.Now(), time.Now())
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("writers blocked behind the retention lock")
	}
	a.retentionMu.Unlock()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	logs, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != existing+writers {
		t.Fatalf("files %d, want %d", len(logs), existing+writers)
	}
	// Under the limit no write scans the directory.
	if a.sweeping.Load() || a.lastSweep.Load() != lastSweep {
		t.Fatal("a write under the size limit started a retention sweep")
	}
	if got := a.totalBytes.Load(); got <= existing {
		t.Fatalf("running total %d did not grow", got)
	}
}

func TestRequestArchiveBackgroundRetentionPrunesOverLimit(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxSizeGB: 1})
	if a.startupErr != nil {
		t.Fatal(a.startupErr)
	}
	dir := a.file.logsDir
	// Two sparse files sit just under the 1 GB limit, so the next write crosses it.
	const sparse = 499999990
	old := time.Now().Add(-time.Hour)
	oldest := filepath.Join(dir, archiveFilePrefix+"oldest.log")
	older := filepath.Join(dir, archiveFilePrefix+"older.log")
	for i, p := range []string{oldest, older} {
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(p, sparse); err != nil {
			t.Fatal(err)
		}
		ts := old.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.retain(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest); err != nil {
		t.Fatal("a sweep pruned an archive under its limit")
	}
	if err := a.LogRequest("/v1/chat/completions", "POST", nil, []byte("request"), 200, nil, []byte("response"), nil, nil, nil, nil, nil, "id", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	a.sweeps.Wait()
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatal("oldest log retained over the size limit")
	}
	if _, err := os.Stat(older); err != nil {
		t.Fatal("retention removed more than it needed")
	}
	logs, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("files %d, want the newer log and the new record", len(logs))
	}
	if got := a.totalBytes.Load(); got <= sparse || got > sparse+100000 {
		t.Fatalf("running total %d not resynchronized", got)
	}
}
