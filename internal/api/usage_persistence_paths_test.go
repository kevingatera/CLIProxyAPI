package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestResolveUsageStatisticsPath_DefaultsToLogsDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	authDir := filepath.Join(t.TempDir(), "auth")
	cfg := &config.Config{AuthDir: authDir}

	got := resolveUsageStatisticsPath(cfg, configPath)
	if filepath.Base(got) != usageStatisticsFileName {
		t.Fatalf("expected usage stats filename, got %q", got)
	}
	if filepath.Base(filepath.Dir(got)) != "logs" {
		t.Fatalf("expected usage stats under a logs directory, got %q", got)
	}
	if got == filepath.Join(authDir, usageStatisticsFileName) {
		t.Fatalf("usage stats path should not be stored directly in auth dir: %q", got)
	}
}

func TestResolveUsageStatisticsPath_UsesExplicitRelativePath(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{UsageStatisticsFile: filepath.Join("state", usageStatisticsFileName)}

	got := resolveUsageStatisticsPath(cfg, configPath)
	want := filepath.Join(filepath.Dir(configPath), "state", usageStatisticsFileName)
	if got != want {
		t.Fatalf("unexpected explicit usage stats path: got %q want %q", got, want)
	}
}

func TestMigrateUsageStatisticsFile_MovesLegacyFile(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "auth", usageStatisticsFileName)
	desiredPath := filepath.Join(dir, "logs", usageStatisticsFileName)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatalf("failed to create legacy dir: %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte(`{"usage":true}`), 0o600); err != nil {
		t.Fatalf("failed to write legacy usage stats: %v", err)
	}

	if err := migrateUsageStatisticsFile(legacyPath, desiredPath); err != nil {
		t.Fatalf("migrateUsageStatisticsFile() error: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy file to be removed, stat err: %v", err)
	}
	data, err := os.ReadFile(desiredPath)
	if err != nil {
		t.Fatalf("failed to read migrated file: %v", err)
	}
	if string(data) != `{"usage":true}` {
		t.Fatalf("unexpected migrated content: %s", string(data))
	}
}

func TestConfigureUsagePersistenceDoesNotOverwriteRejectedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), usageStatisticsFileName)
	const rejected = `{"version":9,"usage":{"total_requests":1}}`
	if err := os.WriteFile(path, []byte(rejected), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	s := &Server{}
	cfg := &config.Config{UsageStatisticsEnabled: true, UsageStatisticsPersist: true, UsageStatisticsFile: path}
	s.configureUsagePersistence(nil, cfg)
	if s.usagePersistence != nil {
		s.usagePersistence.Stop()
		s.usagePersistence = nil
	}
	if got, _ := os.ReadFile(path); string(got) != rejected {
		t.Fatalf("rejected snapshot was overwritten: %s", got)
	}
}

func TestMigrateUsageStatisticsFile_CopyFallbackCommitsThenRemovesLegacy(t *testing.T) {
	prev := renameLegacyFile
	renameLegacyFile = func(string, string) error { return errors.New("cross-device link") }
	defer func() { renameLegacyFile = prev }()

	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "auth", usageStatisticsFileName)
	desiredPath := filepath.Join(dir, "logs", usageStatisticsFileName)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const payload = `{"usage":true,"pad":"0123456789"}`
	if err := os.WriteFile(legacyPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := migrateUsageStatisticsFile(legacyPath, desiredPath); err != nil {
		t.Fatalf("migrateUsageStatisticsFile() error: %v", err)
	}
	if got, _ := os.ReadFile(desiredPath); string(got) != payload {
		t.Fatalf("migrated content = %q", got)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy file should be removed after commit, stat err: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(desiredPath))
	if len(entries) != 1 {
		t.Fatalf("temp files left in destination dir: %v", entries)
	}
}

func TestMigrateUsageStatisticsFile_KeepsLegacyWhenCommitFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directory does not block root")
	}
	prev := renameLegacyFile
	renameLegacyFile = func(string, string) error { return errors.New("cross-device link") }
	defer func() { renameLegacyFile = prev }()

	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "auth", usageStatisticsFileName)
	desiredDir := filepath.Join(dir, "logs")
	desiredPath := filepath.Join(desiredDir, usageStatisticsFileName)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const payload = `{"usage":true}`
	if err := os.WriteFile(legacyPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(desiredDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(desiredDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(desiredDir, 0o700) }()

	if err := migrateUsageStatisticsFile(legacyPath, desiredPath); err == nil {
		t.Fatal("expected commit error")
	}
	if got, _ := os.ReadFile(legacyPath); string(got) != payload {
		t.Fatalf("legacy file changed after failed commit: %q", got)
	}
	if _, err := os.Stat(desiredPath); !os.IsNotExist(err) {
		t.Fatalf("destination exists after failed commit, stat err: %v", err)
	}
}
