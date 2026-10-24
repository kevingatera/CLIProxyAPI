package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// snapshotVersion is the current on-disk and export format. Version 1 files
// (written by earlier builds, with raw keys) are migrated on load.
const snapshotVersion = 2

// renameFile is swapped in tests to simulate a failed commit.
var renameFile = os.Rename

// errSnapshotNotLoaded is returned by Flush when the existing file did not load.
// Writing over it would destroy data this process never read.
var errSnapshotNotLoaded = errors.New("usage snapshot was not loaded; refusing to overwrite it")

type persistedSnapshot struct {
	Version int                `json:"version"`
	SavedAt time.Time          `json:"saved_at"`
	Usage   StatisticsSnapshot `json:"usage"`
}

// EncodeSnapshot serializes a snapshot in the current container format.
func EncodeSnapshot(snapshot StatisticsSnapshot) ([]byte, error) {
	return json.Marshal(persistedSnapshot{
		Version: snapshotVersion,
		SavedAt: time.Now().UTC(),
		Usage:   snapshot,
	})
}

// DecodeSnapshot parses a versioned container (v1 or v2) or a bare legacy snapshot.
// It normalizes raw key material to fingerprints. needsRewrite reports that the
// input was not in the current format or held raw keys, so the caller should
// write it back.
func DecodeSnapshot(data []byte) (snapshot StatisticsSnapshot, needsRewrite bool, err error) {
	var probe struct {
		Version *int            `json:"version"`
		Usage   json.RawMessage `json:"usage"`
	}
	if err = json.Unmarshal(data, &probe); err != nil {
		return StatisticsSnapshot{}, false, fmt.Errorf("decode usage snapshot: %w", err)
	}

	if probe.Usage == nil {
		// Bare snapshot from before the versioned container existed.
		if err = json.Unmarshal(data, &snapshot); err != nil {
			return StatisticsSnapshot{}, false, fmt.Errorf("decode legacy usage snapshot: %w", err)
		}
		snapshot, _ = normalizeSnapshot(snapshot)
		return snapshot, true, nil
	}

	version := 0
	if probe.Version != nil {
		version = *probe.Version
	}
	if version < 0 || version > snapshotVersion {
		return StatisticsSnapshot{}, false, fmt.Errorf("unsupported usage snapshot version %d", version)
	}
	if err = json.Unmarshal(probe.Usage, &snapshot); err != nil {
		return StatisticsSnapshot{}, false, fmt.Errorf("decode usage snapshot body: %w", err)
	}
	snapshot, changed := normalizeSnapshot(snapshot)
	return snapshot, changed || version != snapshotVersion, nil
}

// Persistence periodically writes the usage aggregate to disk.
// It is safe to Start/Stop multiple times.
type Persistence struct {
	mu      sync.Mutex
	writeMu sync.Mutex

	stats    *RequestStatistics
	path     string
	interval time.Duration

	// blocked is set when Load failed. A blocked Persistence never writes the file.
	blocked bool

	stop chan struct{}
	done chan struct{}
}

// PersistenceConfig configures a Persistence.
type PersistenceConfig struct {
	Stats    *RequestStatistics
	Path     string
	Interval time.Duration
}

// NewPersistence returns a Persistence for cfg. Call Load before Start.
func NewPersistence(cfg PersistenceConfig) *Persistence {
	return &Persistence{
		stats:    cfg.Stats,
		path:     strings.TrimSpace(cfg.Path),
		interval: cfg.Interval,
	}
}

// Load merges the snapshot at path into the aggregate. A missing file is not an error.
// Legacy or raw-key snapshots are migrated once. The migration is committed to disk
// before the aggregate changes, so a failed Load leaves both untouched and the
// next Load can retry. A failed Load blocks Flush.
func (p *Persistence) Load() error {
	if p == nil || p.stats == nil || p.path == "" {
		return nil
	}
	err := p.load()
	p.mu.Lock()
	p.blocked = err != nil
	p.mu.Unlock()
	return err
}

func (p *Persistence) load() error {
	data, err := os.ReadFile(p.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read usage snapshot %s: %w", p.path, err)
	}
	snapshot, needsRewrite, err := DecodeSnapshot(data)
	if err != nil {
		return fmt.Errorf("load usage snapshot %s: %w", p.path, err)
	}
	if needsRewrite {
		encoded, err := EncodeSnapshot(snapshot)
		if err != nil {
			return fmt.Errorf("encode migrated usage snapshot: %w", err)
		}
		if err := writeFileAtomic(p.path, encoded, 0o600); err != nil {
			return fmt.Errorf("migrate usage snapshot %s: %w", p.path, err)
		}
		log.Info("usage: migrated persisted snapshot to the fingerprinted format")
	}
	if _, err := p.stats.restoreSnapshot(p.path, snapshot); err != nil {
		return fmt.Errorf("load usage snapshot %s: %w", p.path, err)
	}
	return nil
}

// Flush writes the current aggregate to disk atomically.
func (p *Persistence) Flush() error {
	if p == nil || p.stats == nil || p.path == "" {
		return nil
	}
	p.mu.Lock()
	blocked := p.blocked
	p.mu.Unlock()
	if blocked {
		return errSnapshotNotLoaded
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	data, err := EncodeSnapshot(p.stats.Snapshot())
	if err != nil {
		return fmt.Errorf("encode usage snapshot: %w", err)
	}
	return writeFileAtomic(p.path, data, 0o600)
}

// WriteFileAtomic replaces path with data through a same-directory temp file, fsync,
// and rename. On failure path is unchanged and no temp file is left behind.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm)
}

// writeFileAtomic writes data to a unique temp file in the target directory, fsyncs it,
// and renames it over path. On any failure the destination is left unchanged.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create usage snapshot dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create usage snapshot temp file: %w", err)
	}
	tmpName := tmp.Name()

	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = renameFile(tmpName, path)
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write usage snapshot: %w", err)
	}
	if d, errOpen := os.Open(dir); errOpen == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Start begins periodic flushing in a background goroutine.
func (p *Persistence) Start() {
	if p == nil || p.stats == nil || p.path == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stop != nil || p.blocked {
		return
	}
	if p.interval <= 0 {
		p.interval = 60 * time.Second
	}
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	interval, stop, done := p.interval, p.stop, p.done

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
				if err := p.Flush(); err != nil {
					log.WithError(err).Warn("usage: failed to persist snapshot on shutdown")
				}
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
	stop, done := p.stop, p.done
	p.stop, p.done = nil, nil
	p.mu.Unlock()

	if stop == nil {
		return
	}
	close(stop)
	<-done
}
