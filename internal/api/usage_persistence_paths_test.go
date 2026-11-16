package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestResolveUsageStatisticsPathDefaultsBesideConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	got := resolveUsageStatisticsPath(&config.Config{}, configPath)
	want := filepath.Join(filepath.Dir(configPath), usageStatisticsFileName)
	if got != want {
		t.Fatalf("default path = %q, want %q", got, want)
	}
}

func TestResolveUsageStatisticsPathHonorsExplicitFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	absolute := filepath.Join(t.TempDir(), "custom.json")

	if got := resolveUsageStatisticsPath(&config.Config{UsageStatisticsFile: "data/usage.json"}, configPath); got != filepath.Join(filepath.Dir(configPath), "data", "usage.json") {
		t.Fatalf("relative path = %q", got)
	}
	if got := resolveUsageStatisticsPath(&config.Config{UsageStatisticsFile: absolute}, configPath); got != absolute {
		t.Fatalf("absolute path = %q, want %q", got, absolute)
	}
}

// TestConfigureUsagePersistenceDoesNotOverwriteRejectedSnapshot guards the all-or-nothing
// load: a snapshot that fails to decode is left as it is, and nothing starts writing.
func TestConfigureUsagePersistenceDoesNotOverwriteRejectedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), usageStatisticsFileName)
	const corrupt = `{"version":9,"usage":{}}`
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	server := &Server{}
	server.configureUsagePersistence(&config.Config{
		UsageStatisticsEnabled: true,
		UsageStatisticsPersist: true,
		UsageStatisticsFile:    path,
	})
	t.Cleanup(func() { server.shutdownUsagePersistence() })

	if server.usagePersistence != nil {
		t.Fatal("persistence started on a rejected snapshot")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != corrupt {
		t.Fatalf("rejected snapshot was modified: %s", data)
	}
}
