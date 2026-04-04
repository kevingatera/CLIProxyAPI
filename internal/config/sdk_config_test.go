package config

import "testing"

func TestAPIKeyFingerprintIsStable(t *testing.T) {
	got := APIKeyFingerprint("example-key")
	if got != "b1d20dc3" {
		t.Fatalf("unexpected fingerprint: %s", got)
	}
}

func TestPruneAPIKeyLabelsRemovesUnknownEntries(t *testing.T) {
	cfg := &SDKConfig{
		APIKeys: []string{"alpha", "beta"},
		APIKeyLabels: map[string]string{
			APIKeyFingerprint("alpha"): "Alpha Label",
			APIKeyFingerprint("ghost"): "Ghost Label",
		},
	}

	cfg.PruneAPIKeyLabels()

	if len(cfg.APIKeyLabels) != 1 {
		t.Fatalf("expected 1 label after prune, got %d", len(cfg.APIKeyLabels))
	}
	if cfg.APIKeyLabels[APIKeyFingerprint("alpha")] != "Alpha Label" {
		t.Fatalf("expected alpha label to remain")
	}
}
