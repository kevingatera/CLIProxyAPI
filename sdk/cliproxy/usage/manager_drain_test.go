package usage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// TestManagerDrainWaitsForAdmittedPublisher holds a publisher between admission and queue
// insertion. Drain must wait for that publisher, so the record is delivered before Drain
// returns. Without the admission fence, Drain returns at once and the record is missed.
func TestManagerDrainWaitsForAdmittedPublisher(t *testing.T) {
	const heldModel = "held-between-admission-and-insert"
	paused := make(chan struct{}, 1)
	resume := make(chan struct{})
	prevHook := publishAdmittedHook
	publishAdmittedHook = func(record Record) {
		if record.Model != heldModel {
			return
		}
		paused <- struct{}{}
		<-resume
	}
	defer func() { publishAdmittedHook = prevHook }()

	m := NewManager(8)
	plugin := &countingPlugin{model: heldModel}
	m.Register(plugin)
	m.Start(context.Background())

	go m.Publish(context.Background(), Record{Model: heldModel})
	<-paused

	drainDone := make(chan struct{})
	go func() {
		m.Drain()
		close(drainDone)
	}()

	// The publisher is still paused before insertion, so a correct Drain cannot return.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-drainDone:
		close(resume)
		t.Fatal("Drain returned while an admitted publisher had not queued its record")
	case <-timer.C:
	}

	close(resume)
	<-drainDone
	if got := plugin.count(); got != 1 {
		t.Fatalf("record admitted before Drain was delivered %d times when Drain returned, want 1", got)
	}
	m.Stop()
}

// countingPlugin counts deliveries for one model.
type countingPlugin struct {
	model string
	mu    sync.Mutex
	n     int
}

func (p *countingPlugin) HandleUsage(_ context.Context, record Record) {
	if record.Model != p.model {
		return
	}
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
}

func (p *countingPlugin) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}
