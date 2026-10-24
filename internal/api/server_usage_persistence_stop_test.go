package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// TestServerStopFlushesUsagePersistence guards normal shutdown: usage recorded after
// the last periodic flush must still reach disk when the server stops.
func TestServerStopFlushesUsagePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), usageStatisticsFileName)
	cfg := &config.Config{
		UsageStatisticsEnabled:             true,
		UsageStatisticsPersist:             true,
		UsageStatisticsFile:                path,
		UsageStatisticsSaveIntervalSeconds: 3600,
	}

	server := &Server{}
	server.configureUsagePersistence(nil, cfg)
	if server.usagePersistence == nil {
		t.Fatal("usage persistence was not started")
	}

	const model = "shutdown-flush-regression-model"
	usage.GetRequestStatistics().Record(context.Background(), coreusage.Record{
		Model:  model,
		APIKey: "shutdown-flush-regression-key",
	})

	if errStop := server.Stop(context.Background()); errStop != nil {
		t.Fatalf("server.Stop() error: %v", errStop)
	}

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("expected shutdown to flush usage snapshot, read error: %v", errRead)
	}
	snapshot, _, errDecode := usage.DecodeSnapshot(data)
	if errDecode != nil {
		t.Fatalf("decode flushed snapshot: %v", errDecode)
	}
	found := false
	for _, apiSnapshot := range snapshot.APIs {
		if _, ok := apiSnapshot.Models[model]; ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("flushed snapshot does not contain usage recorded before shutdown: %s", string(data))
	}
	if server.usagePersistence != nil {
		t.Fatal("server.Stop() left usage persistence running")
	}
}
