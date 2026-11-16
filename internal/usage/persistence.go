package usage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	log "github.com/sirupsen/logrus"
)

// snapshotVersion is the only on-disk and import format. Files in any other shape are
// rejected, never rewritten.
const snapshotVersion = 2

// requiredUsageFields lists the members every usage object must carry.
var requiredUsageFields = []string{
	"total_requests", "success_count", "failure_count", "total_tokens", "apis",
	"requests_by_day", "requests_by_hour", "tokens_by_day", "tokens_by_hour",
}

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

// DecodeSnapshot parses and validates a version 2 container. It returns an error for any
// other shape, including unversioned files, unknown versions, missing fields, failed
// invariants, and stored names that are not fingerprints. It never rewrites input.
func DecodeSnapshot(data []byte) (StatisticsSnapshot, error) {
	var probe struct {
		Version *int            `json:"version"`
		Usage   json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("decode usage snapshot: %w", err)
	}
	if probe.Version == nil || *probe.Version != snapshotVersion {
		return StatisticsSnapshot{}, fmt.Errorf("unsupported usage snapshot: want version %d", snapshotVersion)
	}
	if err := requireUsageFields(probe.Usage); err != nil {
		return StatisticsSnapshot{}, err
	}
	var snapshot StatisticsSnapshot
	if err := json.Unmarshal(probe.Usage, &snapshot); err != nil {
		return StatisticsSnapshot{}, fmt.Errorf("decode usage snapshot body: %w", err)
	}
	if err := validateSnapshot(snapshot); err != nil {
		return StatisticsSnapshot{}, err
	}
	return snapshot, nil
}

func requireUsageFields(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("invalid usage snapshot: missing usage object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errors.New("invalid usage snapshot: usage must be an object")
	}
	for _, name := range requiredUsageFields {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("invalid usage snapshot: missing usage.%s", name)
		}
	}
	return nil
}

// validateSnapshot checks the invariants a snapshot written by this code always holds.
func validateSnapshot(s StatisticsSnapshot) error {
	if s.TotalRequests < 0 || s.SuccessCount < 0 || s.FailureCount < 0 || s.TotalTokens < 0 {
		return errors.New("invalid usage snapshot: negative totals")
	}
	if s.SuccessCount+s.FailureCount != s.TotalRequests {
		return errors.New("invalid usage snapshot: success and failure counts do not sum to total_requests")
	}
	if sumCounts(s.RequestsByDay) != s.TotalRequests || sumCounts(s.RequestsByHour) != s.TotalRequests {
		return errors.New("invalid usage snapshot: request counts by day or hour do not match total_requests")
	}
	if sumCounts(s.TokensByDay) != s.TotalTokens || sumCounts(s.TokensByHour) != s.TotalTokens {
		return errors.New("invalid usage snapshot: token counts by day or hour do not match total_tokens")
	}

	var apiRequests, apiTokens int64
	seen := make(map[string]struct{})
	for name, api := range s.APIs {
		if !validAPIName(name) {
			return errors.New("invalid usage snapshot: api key name is not a fingerprint")
		}
		var modelRequests, modelTokens int64
		for _, model := range api.Models {
			if model.TotalRequests < 0 || model.TotalTokens < 0 {
				return errors.New("invalid usage snapshot: negative model totals")
			}
			modelRequests += model.TotalRequests
			modelTokens += model.TotalTokens
			for _, detail := range model.Details {
				if strings.TrimSpace(detail.RequestID) == "" || detail.Timestamp.IsZero() {
					return errors.New("invalid usage snapshot: detail lacks request_id or timestamp")
				}
				if _, dup := seen[detail.RequestID]; dup {
					return errors.New("invalid usage snapshot: duplicate request_id")
				}
				seen[detail.RequestID] = struct{}{}
				if !validSourceName(detail.AuthType, detail.Source) {
					return errors.New("invalid usage snapshot: api key source is not a fingerprint")
				}
			}
		}
		if api.TotalRequests != modelRequests || api.TotalTokens != modelTokens {
			return errors.New("invalid usage snapshot: api totals do not match its models")
		}
		apiRequests += api.TotalRequests
		apiTokens += api.TotalTokens
	}
	if apiRequests != s.TotalRequests || apiTokens != s.TotalTokens {
		return errors.New("invalid usage snapshot: api totals do not match total_requests or total_tokens")
	}
	return nil
}

func sumCounts(values map[string]int64) int64 {
	var total int64
	for _, v := range values {
		total += v
	}
	return total
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
// A file that fails validation is an error and is left untouched. A failed Load blocks Flush.
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
	snapshot, err := DecodeSnapshot(data)
	if err != nil {
		return fmt.Errorf("load usage snapshot %s: %w", p.path, err)
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
				return
			}
		}
	}()
}

// Stop stops periodic flushing and performs a final Flush. The caller must stop
// collection first so that the final snapshot is complete.
func (p *Persistence) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	stop, done := p.stop, p.done
	p.stop, p.done = nil, nil
	p.mu.Unlock()

	if stop != nil {
		close(stop)
		<-done
	}
	if err := p.Flush(); err != nil {
		log.WithError(err).Warn("usage: failed to persist snapshot on shutdown")
	}
}
