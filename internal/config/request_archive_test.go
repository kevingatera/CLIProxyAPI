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
