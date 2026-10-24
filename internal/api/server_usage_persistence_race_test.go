package api

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestUsagePersistenceReloadAndStopAreSerialized runs config reloads and shutdown
// concurrently. Run with -race: the persistence handle must be guarded. After shutdown,
// a reload must not start persistence again, or a writer would outlive the server.
func TestUsagePersistenceReloadAndStopAreSerialized(t *testing.T) {
	cfg := &config.Config{
		UsageStatisticsEnabled:             true,
		UsageStatisticsPersist:             true,
		UsageStatisticsFile:                filepath.Join(t.TempDir(), usageStatisticsFileName),
		UsageStatisticsSaveIntervalSeconds: 3600,
	}
	server := &Server{}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			// A nil old config forces a restart on every call.
			server.configureUsagePersistence(nil, cfg)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = server.Stop(context.Background())
		}
	}()
	wg.Wait()

	if errStop := server.Stop(context.Background()); errStop != nil {
		t.Fatalf("Stop: %v", errStop)
	}
	server.configureUsagePersistence(nil, cfg)
	if server.usagePersistence != nil {
		t.Fatal("a reload after shutdown started usage persistence again")
	}
}
