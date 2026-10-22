package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

// Settings-only and global-exclusion-only edits leave credentials unchanged, so
// only a forced auth refresh re-registers their models.
func TestReloadConfigForcesAuthRefreshForSettingsAndGlobalExclusions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"oauth-settings", func(cfg *config.Config) {
			cfg.OAuthSettings = map[string][]config.OAuthModelSetting{"claude": {{Name: "model-a", Alias: "model-a-long"}}}
		}},
		{"global-excluded-models", func(cfg *config.Config) {
			cfg.GlobalExcludedModels = []string{"model-a"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			authDir := filepath.Join(tmpDir, "auth")
			if err := os.MkdirAll(authDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(authDir, "claude.json"), []byte(`{"type":"claude","email":"a@example.com"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			base := func() *config.Config {
				return &config.Config{AuthDir: authDir, CredentialInFlight: config.DefaultCredentialInFlightConfig()}
			}
			updated := base()
			tc.mutate(updated)
			configPath := filepath.Join(tmpDir, "config.yaml")
			data, err := yaml.Marshal(updated)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(configPath, data, 0o644); err != nil {
				t.Fatal(err)
			}

			queue := make(chan AuthUpdate, 8)
			w := &Watcher{configPath: configPath, authDir: authDir, lastAuthHashes: make(map[string]string)}
			w.SetConfig(base())
			w.SetAuthUpdateQueue(queue)
			defer w.stopDispatch()
			next := func(stage string) AuthUpdate {
				t.Helper()
				select {
				case update := <-queue:
					return update
				case <-time.After(2 * time.Second):
					t.Fatalf("%s: timed out waiting for auth update", stage)
				}
				return AuthUpdate{}
			}

			w.refreshAuthState(false)
			initial := next("initial load")
			if ok := w.reloadConfig(); !ok {
				t.Fatal("expected reloadConfig to succeed")
			}
			if reloaded := next("config reload"); reloaded.ID != initial.ID {
				t.Fatalf("reloaded auth ID = %q, want %q", reloaded.ID, initial.ID)
			}
		})
	}
}
