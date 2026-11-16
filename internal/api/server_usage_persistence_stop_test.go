package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// shutdownGatePlugin holds the usage dispatcher on a gate record until release is closed.
type shutdownGatePlugin struct {
	model   string
	entered chan struct{}
	release chan struct{}
}

func (p *shutdownGatePlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	if record.Model != p.model {
		return
	}
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.release
}

func persistenceConfig(path string) *config.Config {
	return &config.Config{
		UsageStatisticsEnabled:             true,
		UsageStatisticsPersist:             true,
		UsageStatisticsFile:                path,
		UsageStatisticsSaveIntervalSeconds: 3600,
	}
}

func decodeUsageFile(t *testing.T, path string) usage.StatisticsSnapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	snapshot, err := usage.DecodeSnapshot(data)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return snapshot
}

func modelTotal(snapshot usage.StatisticsSnapshot, model string) int64 {
	var total int64
	for _, api := range snapshot.APIs {
		total += api.Models[model].TotalRequests
	}
	return total
}

// TestServerStopFlushesUsagePersistence guards normal shutdown: usage recorded after
// the last periodic flush must still reach disk when the server stops.
func TestServerStopFlushesUsagePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), usageStatisticsFileName)
	server := &Server{}
	server.configureUsagePersistence(persistenceConfig(path))
	if server.usagePersistence == nil {
		t.Fatal("usage persistence was not started")
	}

	const model = "shutdown-flush-regression-model"
	usage.GetRequestStatistics().RecordCollected(context.Background(), coreusage.Record{
		RequestID: "shutdown-flush-1", Model: model, APIKey: "shutdown-flush-key", RequestedAt: time.Now(),
	})

	if errStop := server.Stop(context.Background()); errStop != nil {
		t.Fatalf("server.Stop() error: %v", errStop)
	}
	if got := modelTotal(decodeUsageFile(t, path), model); got != 1 {
		t.Fatalf("flushed snapshot holds %d requests for %s, want 1", got, model)
	}
	if server.usagePersistence != nil {
		t.Fatal("server.Stop() left usage persistence running")
	}
}

// TestShutdownDeliversQueuedUsageBeforeFinalFlush guards the shutdown order. Records already
// published to the usage queue must reach the aggregate and the final snapshot, even when
// the dispatcher is still busy when shutdown starts.
func TestShutdownDeliversQueuedUsageBeforeFinalFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), usageStatisticsFileName)
	server := &Server{}
	server.configureUsagePersistence(persistenceConfig(path))
	if server.usagePersistence == nil {
		t.Fatal("usage persistence was not started")
	}

	const gateModel = "shutdown-drain-gate-model"
	const queuedModel = "shutdown-drain-queued-model"
	gate := &shutdownGatePlugin{model: gateModel, entered: make(chan struct{}, 1), release: make(chan struct{})}
	coreusage.RegisterNamedPlugin("api-test-shutdown-gate", gate)

	ctx := context.Background()
	now := time.Now()
	coreusage.PublishRecord(ctx, coreusage.Record{RequestID: "gate-1", Model: gateModel, APIKey: "gate-key", RequestedAt: now})
	<-gate.entered
	const queued = 3
	for i := 0; i < queued; i++ {
		coreusage.PublishRecord(ctx, coreusage.Record{
			RequestID: fmt.Sprintf("queued-%d", i), Model: queuedModel, APIKey: "queued-key", RequestedAt: now,
		})
	}

	stopped := make(chan struct{})
	go func() {
		server.shutdownUsagePersistence()
		close(stopped)
	}()
	// Collection must stay on while the gate holds undelivered usage. Without the drain it
	// turns off at once, so the state is observable before the gate opens. The wait is
	// bounded, and it ends on its own when the fixed code keeps collection on.
	stats := usage.GetRequestStatistics()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
wait:
	for stats.Collecting() {
		select {
		case <-deadline.C:
			break wait
		default:
			runtime.Gosched()
		}
	}
	if !stats.Collecting() {
		close(gate.release)
		<-stopped
		t.Fatal("collection stopped before queued usage was delivered")
	}
	close(gate.release)
	<-stopped

	if got := modelTotal(decodeUsageFile(t, path), queuedModel); got != queued {
		t.Fatalf("final snapshot holds %d of %d queued requests", got, queued)
	}
}

// TestPathSwitchStartsFromNewFileAndKeepsOldHistory defines the path-change behaviour. The old
// file is flushed and left alone. The aggregate starts empty from the new file, even when that
// file has pruned history that a non-empty aggregate would reject. Switching back restores the
// first file's history.
func TestPathSwitchStartsFromNewFileAndKeepsOldHistory(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.json")
	pathB := filepath.Join(dir, "b.json")
	pathC := filepath.Join(dir, "c.json")
	server := &Server{configFilePath: filepath.Join(dir, "config.yaml")}
	t.Cleanup(server.shutdownUsagePersistence)
	ctx := context.Background()
	stats := usage.GetRequestStatistics()

	server.configureUsagePersistence(persistenceConfig(pathA))
	stats.RecordCollected(ctx, coreusage.Record{RequestID: "a-1", Model: "model-a", APIKey: "key-a", RequestedAt: time.Now()})
	server.configureUsagePersistence(persistenceConfig(pathB))
	if got := modelTotal(decodeUsageFile(t, pathA), "model-a"); got != 1 {
		t.Fatalf("switching away from A did not flush A: %d", got)
	}
	if got := modelTotal(stats.Snapshot(), "model-a"); got != 0 {
		t.Fatalf("aggregate kept A history after switching to B: %d", got)
	}

	stats.RecordCollected(ctx, coreusage.Record{RequestID: "b-1", Model: "model-b", APIKey: "key-b", RequestedAt: time.Now()})

	// C has only pruned history. Loading it into a non-empty aggregate would be rejected, so
	// this checks that the switch resets first.
	seed := usage.NewRequestStatistics()
	seed.Record(ctx, coreusage.Record{RequestID: "c-old", Model: "model-c", APIKey: "key-c", RequestedAt: time.Now().Add(-8 * 24 * time.Hour)})
	encoded, err := usage.EncodeSnapshot(seed.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathC, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	server.configureUsagePersistence(persistenceConfig(pathC))
	if server.usagePersistence == nil {
		t.Fatal("persistence did not start on a file with pruned history after a path switch")
	}
	if got := modelTotal(stats.Snapshot(), "model-b"); got != 0 {
		t.Fatalf("aggregate kept B history after switching to C: %d", got)
	}
	if got := modelTotal(decodeUsageFile(t, pathB), "model-b"); got != 1 {
		t.Fatalf("B file lacks its history after switching away: %d", got)
	}

	server.configureUsagePersistence(persistenceConfig(pathA))
	if got := modelTotal(stats.Snapshot(), "model-a"); got != 1 {
		t.Fatalf("switching back to A did not restore its history: %d", got)
	}
	if got := modelTotal(stats.Snapshot(), "model-c"); got != 0 {
		t.Fatalf("aggregate kept C history after switching to A: %d", got)
	}
}
