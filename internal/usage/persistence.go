package usage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type persistedSnapshot struct {
	Version int                `json:"version"`
	SavedAt time.Time          `json:"saved_at"`
	Usage   StatisticsSnapshot `json:"usage"`
}

// Persistence periodically persists in-memory usage statistics to disk.
// It is safe to Start/Stop multiple times.
type Persistence struct {
	mu sync.Mutex

	stats    *RequestStatistics
	path     string
	interval time.Duration

	stop chan struct{}
	done chan struct{}
}

type PersistenceConfig struct {
	Stats    *RequestStatistics
	Path     string
	Interval time.Duration
}

func NewPersistence(cfg PersistenceConfig) *Persistence {
	return &Persistence{
		stats:    cfg.Stats,
		path:     cfg.Path,
		interval: cfg.Interval,
	}
}

// Load reads the persisted snapshot from disk (if present) and merges it into stats.
func (p *Persistence) Load() error {
	if p == nil || p.stats == nil {
		return nil
	}
	path := strings.TrimSpace(p.path)
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var container persistedSnapshot
	if err := json.Unmarshal(data, &container); err == nil && (container.Version == 1 || container.Version == 0) {
		p.stats.MergeSnapshot(container.Usage)
		return nil
	}

	var snapshot StatisticsSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	p.stats.MergeSnapshot(snapshot)
	return nil
}

// Flush writes the current snapshot to disk immediately.
func (p *Persistence) Flush() error {
	if p == nil || p.stats == nil {
		return nil
	}
	path := strings.TrimSpace(p.path)
	if path == "" {
		return nil
	}

	snapshot := p.stats.Snapshot()
	payload := persistedSnapshot{
		Version: 1,
		SavedAt: time.Now().UTC(),
		Usage:   snapshot,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(path)
		return os.Rename(tmp, path)
	}
	return nil
}

// Start begins periodic flushing in a background goroutine.
func (p *Persistence) Start() {
	if p == nil || p.stats == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stop != nil {
		return
	}
	if strings.TrimSpace(p.path) == "" {
		return
	}
	if p.interval <= 0 {
		p.interval = 60 * time.Second
	}

	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	interval := p.interval
	stop := p.stop
	done := p.done

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := p.Flush(); err != nil {
					log.WithError(err).Warn("usage: failed to persist snapshot")
				}
			case <-stop:
				_ = p.Flush()
				return
			}
		}
	}()
}

// Stop stops periodic flushing and performs a final Flush.
func (p *Persistence) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	stop := p.stop
	done := p.done
	p.stop = nil
	p.done = nil
	p.mu.Unlock()

	if stop == nil {
		return
	}
	close(stop)
	<-done
}
