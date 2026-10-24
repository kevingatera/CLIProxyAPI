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

// TestReloadEnablingArchiveKeepsLegacyLoggerDisabled guards the opposite
// transition: enabling request-archive on a server that started with it off must
// not switch on the legacy file logger through the combined archive state.
// Archive mode itself only starts on the restart documented in
// docs/request-archive.md.
func TestReloadEnablingArchiveKeepsLegacyLoggerDisabled(t *testing.T) {
	cfg := &config.Config{}
	server := newTestServerWithConfig(t, cfg)

	legacy, ok := server.requestLogger.(*logging.FileRequestLogger)
	if !ok {
		t.Fatalf("request logger = %T, want *logging.FileRequestLogger", server.requestLogger)
	}
	if legacy.IsEnabled() {
		t.Fatal("legacy request logger should start disabled")
	}

	// Old runtime state: archive off and request-log off.
	oldCfg := *cfg
	server.oldConfigYaml, _ = yaml.Marshal(&oldCfg)

	// Reload with the archive enabled while request-log stays off.
	newCfg := oldCfg
	newCfg.RequestArchive = config.RequestArchiveConfig{Enabled: true, Directory: t.TempDir()}

	if okUpdate := server.UpdateClientsContext(context.Background(), &newCfg); !okUpdate {
		t.Fatal("UpdateClientsContext returned false")
	}
	if legacy.IsEnabled() {
		t.Fatal("enabling the request archive switched on the legacy file logger")
	}

	// The archive intent survives the reload so a restart enters archive mode.
	server.cfgMu.RLock()
	active := server.cfg
	server.cfgMu.RUnlock()
	if active == nil || !active.RequestArchive.Enabled {
		t.Fatal("reload dropped the enabled request archive")
	}
	restarted, ok := defaultRequestLoggerFactory(active, "config.yaml").(*logging.RequestArchive)
	if !ok {
		t.Fatal("restarted request logger is not an archive")
	}
	if !restarted.IsEnabled() {
		t.Fatal("restart with the reloaded config did not enable the request archive")
	}
}
