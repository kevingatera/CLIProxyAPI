package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeConfigLayoutKeepsUsagePersistenceKeys guards the v8 write path: a
// management write must keep usage persistence settings instead of commenting them out.
func TestNormalizeConfigLayoutKeepsUsagePersistenceKeys(t *testing.T) {
	legacy := []byte("port: 8317\n" +
		"usage-statistics-enabled: true\n" +
		"usage-statistics-persist: true\n" +
		"usage-statistics-file: /data/usage_stats.json\n" +
		"usage-statistics-save-interval-seconds: 120\n")

	out, changed, err := NormalizeConfigLayout(legacy, true)
	if err != nil {
		t.Fatalf("NormalizeConfigLayout: %v", err)
	}
	if !changed {
		t.Fatal("expected the legacy layout to change")
	}
	if strings.Contains(string(out), "# usage-statistics") {
		t.Fatalf("usage persistence keys were commented out during v8 migration:\n%s", out)
	}
	if err := ValidateV8Config(out); err != nil {
		t.Fatalf("ValidateV8Config(migrated) error: %v\n%s", err, out)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(migrated): %v\n%s", err, out)
	}
	if !cfg.UsageStatisticsPersist {
		t.Fatalf("usage-statistics-persist lost after migration:\n%s", out)
	}
	if cfg.UsageStatisticsFile != "/data/usage_stats.json" {
		t.Fatalf("usage-statistics-file = %q after migration", cfg.UsageStatisticsFile)
	}
	if cfg.UsageStatisticsSaveIntervalSeconds != 120 {
		t.Fatalf("usage-statistics-save-interval-seconds = %d after migration", cfg.UsageStatisticsSaveIntervalSeconds)
	}
}
