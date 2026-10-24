package api

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"gopkg.in/yaml.v3"
)

// TestReloadKeepsExplicitlyEnabledArchiveWhenRequestLogTurnsOff guards the reload
// path: turning request-log off must not disable a request archive that is
// explicitly enabled through request-archive.
func TestReloadKeepsExplicitlyEnabledArchiveWhenRequestLogTurnsOff(t *testing.T) {
	cfg := &config.Config{
		SDKConfig:      config.SDKConfig{RequestLog: true},
		RequestArchive: config.RequestArchiveConfig{Enabled: true, Directory: t.TempDir()},
	}
	server := newTestServerWithConfig(t, cfg)

	archive, ok := server.requestLogger.(*logging.RequestArchive)
	if !ok {
		t.Fatalf("request logger = %T, want *logging.RequestArchive", server.requestLogger)
	}
	if !archive.IsEnabled() {
		t.Fatal("request archive should start enabled")
	}

	// Old runtime state: request-log on and the archive on.
	oldCfg := *cfg
	server.oldConfigYaml, _ = yaml.Marshal(&oldCfg)

	// Reload with request-log off while request-archive stays explicitly enabled.
	newCfg := oldCfg
	newCfg.RequestLog = false

	if okUpdate := server.UpdateClientsContext(context.Background(), &newCfg); !okUpdate {
		t.Fatal("UpdateClientsContext returned false")
	}
	if !archive.IsEnabled() {
		t.Fatal("reload with request-log off disabled an explicitly enabled request archive")
	}
}
