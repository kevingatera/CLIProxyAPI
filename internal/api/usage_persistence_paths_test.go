package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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
