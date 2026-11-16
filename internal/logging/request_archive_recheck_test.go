package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Retention in a marked archive directory must not reach logs that the archive did not write.
func TestArchiveRetentionMatchesOnlyArchivePrefix(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{RequestArchive: config.RequestArchiveConfig{Enabled: true, Directory: dir, MaxAgeDays: 7}}
	a := NewRequestArchive(cfg, "", dir)
	if a.startupErr != nil {
		t.Fatal(a.startupErr)
	}
	now := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
	a.clock = func() time.Time { return now }
	old := now.Add(-30 * 24 * time.Hour)
	files := map[string]bool{ // name -> expected to survive
		"unrelated-old.log":                      true,
		archiveFilePrefix + "old.log":            false,
		"error-" + archiveFilePrefix + "old.log": false,
	}
	for name := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.retain(); err != nil {
		t.Fatal(err)
	}
	for name, survive := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		if survive && err != nil {
			t.Errorf("unrelated log %s was deleted", name)
		}
		if !survive && !os.IsNotExist(err) {
			t.Errorf("expired archive file %s was retained", name)
		}
	}
}

// The size pass counts and deletes archive files only.
func TestArchiveSizePassSkipsUnrelatedLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	entries := []struct {
		name string
		age  time.Duration
	}{
		{"unrelated.log", 30 * 24 * time.Hour},
		{archiveFilePrefix + "older.log", 20 * 24 * time.Hour},
		{archiveFilePrefix + "newer.log", 10 * 24 * time.Hour},
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.name)
		if err := os.WriteFile(p, []byte(strings.Repeat("x", 100)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-e.age), now.Add(-e.age)); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := enforceLogDirSizeLimitMatching(dir, 150, "", isArchiveLogFileName)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d files, want 1", deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "unrelated.log")); err != nil {
		t.Fatal("size pass deleted an unrelated log")
	}
	if _, err := os.Stat(filepath.Join(dir, archiveFilePrefix+"older.log")); !os.IsNotExist(err) {
		t.Fatal("size pass kept the oldest archive file over the limit")
	}
}

// A non-empty unmarked directory is refused before any permission change.
func TestArchiveRefusedDirectoryKeepsMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RequestArchive: config.RequestArchiveConfig{Enabled: true, Directory: dir}}
	a := NewRequestArchive(cfg, "", dir)
	if a.startupErr == nil {
		t.Fatal("non-empty unmarked directory was accepted")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("refused directory mode changed to %v", info.Mode().Perm())
	}
}

// The stream admits bytes before they are queued, and a shortfall ends the budget
// so the kept transcript stays a prefix.
func TestArchiveStreamReserveBoundsQueuedBytes(t *testing.T) {
	stream := &archiveStream{budget: archiveBudget{remaining: 10}}
	for i := 0; i < 5; i++ {
		chunk := []byte("abcd")
		if kept := stream.ReserveChunk(len(chunk)); kept > 0 {
			stream.WriteChunkAsync(chunk[:kept])
		}
	}
	if got := string(stream.pending); got != "abcdabcdab" {
		t.Fatalf("queued %q, want the 10 byte prefix", got)
	}
	if !stream.pendingCut {
		t.Fatal("shortfall did not record a cut")
	}
	if kept := stream.ReserveChunk(1); kept != 0 {
		t.Fatalf("budget admitted %d bytes after a shortfall", kept)
	}
}

// Keys keep their order and number text when a value is masked, so the Doc edit
// touches only the sensitive value.
func TestArchiveRedactorEditsJSONThroughDocKeepsFormatting(t *testing.T) {
	in := `{"zeta":1.50,"api_key":"plain-secret-value","alpha":12345678901234567890,"nested":{"b":2e3,"text":"hello"}}`
	r := NewArchiveRedactor(nil)
	for name, out := range map[string]string{
		"Bytes":      string(r.Bytes([]byte(in))),
		"Transcript": string(r.Transcript([]byte("data: " + in))),
	} {
		if strings.Contains(out, "plain-secret-value") {
			t.Fatalf("%s leaked the sensitive value: %s", name, out)
		}
		zeta := strings.Index(out, `"zeta":1.50`)
		alpha := strings.Index(out, `"alpha":12345678901234567890`)
		if zeta < 0 || alpha < 0 || zeta > alpha {
			t.Fatalf("%s changed key order or number text: %s", name, out)
		}
		if !strings.Contains(out, `"b":2e3`) {
			t.Fatalf("%s rewrote an unchanged number: %s", name, out)
		}
		if !strings.Contains(out, `"api_key":"[REDACTED]"`) {
			t.Fatalf("%s did not mask the api_key value: %s", name, out)
		}
	}
}

// A document with a duplicate key cannot be edited by path, so it fails closed.
func TestArchiveRedactorFailsClosedOnDuplicateKeys(t *testing.T) {
	r := NewArchiveRedactor(nil)
	if out := string(r.Transcript([]byte(`{"text":"leak-one","text":"leak-two"}`))); strings.Contains(out, "leak-") {
		t.Fatalf("duplicate text keys leaked: %s", out)
	}
	if out := string(r.Bytes([]byte(`{"zeta":1,"api_key":"x","api_key":"plain-dup"}`))); strings.Contains(out, "plain-dup") {
		t.Fatalf("duplicate api_key leaked: %s", out)
	}
}
