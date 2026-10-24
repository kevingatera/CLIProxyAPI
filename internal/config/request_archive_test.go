package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRequestArchiveConfigEnablesProviderCapture(t *testing.T) {
	for _, raw := range []string{"request-archive: {enabled: true, max-size-gb: 30}\n", "observability: {logs: {request-archive: {enabled: true, max-size-gb: 30}}}\n"} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		_ = os.WriteFile(p, []byte(raw), 0600)
		c, err := LoadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		if !c.RequestLog || !c.RequestArchive.Enabled || c.RequestArchive.MaxSizeGB != 30 {
			t.Fatalf("archive not loaded: %#v", c.RequestArchive)
		}
	}
}

func TestRequestArchiveRetentionAndCaptureLimitsLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	raw := "request-archive: {enabled: true, max-age-days: 14, max-capture-mb: 8, max-concurrent-captures: 4, metadata-only: true}\n"
	_ = os.WriteFile(p, []byte(raw), 0600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	rc := c.RequestArchive
	if rc.MaxAgeDays != 14 || rc.MaxCaptureMB != 8 || rc.MaxConcurrentCaptures != 4 || !rc.MetadataOnly {
		t.Fatalf("limits not loaded: %#v", rc)
	}
	_ = os.WriteFile(p, []byte("request-archive: {enabled: true, max-capture-mb: -1}\n"), 0600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("negative capture limit accepted")
	}
}
