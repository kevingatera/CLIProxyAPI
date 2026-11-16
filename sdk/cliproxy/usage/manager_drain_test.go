package usage

import (
	"context"
	"sync/atomic"
	"testing"
)

// drainGatePlugin blocks the dispatcher inside HandleUsage until release is closed.
type drainGatePlugin struct {
	entered chan struct{}
	release chan struct{}
	handled atomic.Bool
}

func (p *drainGatePlugin) HandleUsage(_ context.Context, _ Record) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.release
	p.handled.Store(true)
}

// TestManagerDrainWaitsForQueuedDelivery guards shutdown: records already published must
// reach plugins before a caller treats the stream as complete.
func TestManagerDrainWaitsForQueuedDelivery(t *testing.T) {
	m := NewManager(8)
	plugin := &drainGatePlugin{entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.Register(plugin)
	m.Start(context.Background())

	m.Publish(context.Background(), Record{Model: "drain-test"})
	<-plugin.entered

	drainDone := make(chan struct{})
	go func() {
		m.Drain()
		close(drainDone)
	}()

	// The plugin is still blocked, so Drain cannot have finished.
	close(plugin.release)
	<-drainDone
	if !plugin.handled.Load() {
		t.Fatal("Drain returned before the published record was delivered")
	}
	m.Stop()
}

// TestManagerDrainWithEmptyQueueReturns covers the no-op case.
func TestManagerDrainWithEmptyQueueReturns(t *testing.T) {
	m := NewManager(8)
	m.Drain()
	var nilManager *Manager
	nilManager.Drain()
}
