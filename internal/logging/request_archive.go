package logging

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	log "github.com/sirupsen/logrus"
)

const (
	defaultArchiveMaxSizeGB        = 30
	defaultArchiveCaptureMB        = 32
	defaultArchiveConcurrentCaps   = 16
	archiveMinSecretFragmentLength = 4
	archiveTruncatedMarker         = "[ARCHIVE TRUNCATED: per-request capture limit reached]"
	archiveConcurrencyMarker       = "[ARCHIVE OMITTED: concurrent capture limit reached]"
	archiveMetadataOnlyMarker      = "[ARCHIVE OMITTED: metadata-only mode]"
)

// RequestArchive sanitizes before delegating to disk storage. It intentionally
// does not expose FileBodySource: upstream sections stay in memory until they
// can be redacted, so unmasked credentials never enter temporary log files.
type RequestArchive struct {
	file        *FileRequestLogger
	cfg         atomic.Pointer[config.Config]
	enabled     atomic.Bool
	secretsMu   sync.RWMutex
	secrets     func() []string
	retentionMu sync.Mutex
	startupErr  error
	// activeCaptures counts streaming captures that buffer transcript bytes.
	activeCaptures atomic.Int64
	// clock is replaceable in tests so age retention needs no sleeps.
	clock func() time.Time
}

func NewRequestArchive(cfg *config.Config, configDir, logsDir string) *RequestArchive {
	dir := cfg.RequestArchive.Directory
	if dir == "" {
		dir = filepath.Join(logsDir, "requests")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(configDir, dir)
	}
	a := &RequestArchive{file: NewFileRequestLogger(true, dir, "", 0), clock: time.Now}
	a.startupErr = os.MkdirAll(dir, 0700)
	if a.startupErr == nil {
		a.startupErr = os.Chmod(dir, 0700)
	}
	a.SetConfig(cfg)
	if a.startupErr == nil {
		a.startupErr = a.retain()
	}
	return a
}

func (a *RequestArchive) SetConfig(cfg *config.Config) {
	a.cfg.Store(cfg)
	a.enabled.Store(cfg.RequestLog || cfg.RequestArchive.Enabled)
}
func (a *RequestArchive) SetEnabled(enabled bool) { a.enabled.Store(enabled) }
func (a *RequestArchive) IsEnabled() bool         { return a.enabled.Load() }
func (a *RequestArchive) LosslessStreaming() bool { return true }
func (a *RequestArchive) SetSecretProvider(provider func() []string) {
	a.secretsMu.Lock()
	a.secrets = provider
	a.secretsMu.Unlock()
}

func (a *RequestArchive) redactor(headers map[string][]string) *ArchiveRedactor {
	cfg := a.cfg.Load()
	secrets := ArchiveSecrets(cfg)
	if cfg != nil {
		secrets = append(secrets, cfg.RemoteManagement.SecretKey)
	}
	a.secretsMu.RLock()
	if a.secrets != nil {
		secrets = append(secrets, a.secrets()...)
	}
	a.secretsMu.RUnlock()
	for name, values := range headers {
		if archiveSensitiveName(name) {
			for _, value := range values {
				secrets = append(secrets, value)
				if bytes.EqualFold([]byte(name), []byte("Cookie")) || bytes.EqualFold([]byte(name), []byte("Set-Cookie")) {
					for _, part := range strings.Split(value, ";") {
						if _, credential, ok := strings.Cut(part, "="); ok {
							secrets = append(secrets, strings.TrimSpace(credential))
						}
						if bytes.EqualFold([]byte(name), []byte("Set-Cookie")) {
							break
						}
					}
				}
				if len(value) > 7 && bytes.EqualFold([]byte(value[:7]), []byte("Bearer ")) {
					secrets = append(secrets, value[7:])
				}
			}
		}
	}
	return NewArchiveRedactor(secrets)
}

func (a *RequestArchive) retain() error {
	a.retentionMu.Lock()
	defer a.retentionMu.Unlock()
	return a.retainLocked()
}
func (a *RequestArchive) retainLocked() error {
	cfg := a.cfg.Load()
	if days := cfg.RequestArchive.MaxAgeDays; days > 0 {
		cutoff := a.clock().Add(-time.Duration(days) * 24 * time.Hour)
		if _, err := removeArchiveFilesBefore(a.file.logsDir, cutoff); err != nil {
			return err
		}
	}
	gb := cfg.RequestArchive.MaxSizeGB
	if gb <= 0 {
		gb = defaultArchiveMaxSizeGB
	}
	_, err := enforceLogDirSizeLimit(a.file.logsDir, int64(gb)*1000000000, "")
	return err
}

// removeArchiveFilesBefore deletes completed archive files last modified before
// cutoff. Temporary captures and non-log files are never matched.
func removeArchiveFilesBefore(dir string, cutoff time.Time) (int, error) {
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return 0, nil
		}
		return 0, errRead
	}
	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() || !isLogFileName(entry.Name()) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if errRemove := os.Remove(path); errRemove != nil {
			log.WithError(errRemove).Warnf("logging: failed to remove expired archive file: %s", entry.Name())
			continue
		}
		deleted++
	}
	return deleted, nil
}

// archiveBudget bounds the raw bytes one request may hold in memory.
type archiveBudget struct {
	remaining int64
}

// take returns the prefix of data that still fits and reports whether data was cut.
func (b *archiveBudget) take(data []byte) ([]byte, bool) {
	if int64(len(data)) <= b.remaining {
		b.remaining -= int64(len(data))
		return data, false
	}
	kept := data[:b.remaining]
	b.remaining = 0
	return kept, true
}

// admitRedacted charges masked bytes to the budget. A cut keeps the prefix
// without any trailing fragment of a known secret, then appends the marker.
func admitRedacted(b *archiveBudget, data []byte, secrets []string, marker string) []byte {
	kept, cut := b.take(data)
	if !cut {
		return kept
	}
	return markCut(kept, secrets, marker)
}

func markCut(data []byte, secrets []string, marker string) []byte {
	trimmed := trimSecretFragment(data, secrets)
	out := make([]byte, 0, len(trimmed)+len(marker))
	out = append(out, trimmed...)
	return append(out, marker...)
}

// trimSecretFragment drops a tail that begins a known secret. A cut can land
// inside a credential, and the unmasked prefix must not persist.
func trimSecretFragment(data []byte, secrets []string) []byte {
	cut := len(data)
	for _, secret := range secrets {
		for k := min(len(secret)-1, len(data)); k >= archiveMinSecretFragmentLength; k-- {
			if bytes.HasSuffix(data, []byte(secret[:k])) {
				if len(data)-k < cut {
					cut = len(data) - k
				}
				break
			}
		}
	}
	return data[:cut]
}

// captureBudgetBytes returns the per-request in-memory cap. Zero or unset uses the default.
func captureBudgetBytes(cfg *config.Config) int64 {
	mb := cfg.RequestArchive.MaxCaptureMB
	if mb <= 0 {
		mb = defaultArchiveCaptureMB
	}
	return int64(mb) << 20
}

func concurrentCaptureLimit(cfg *config.Config) int64 {
	if n := cfg.RequestArchive.MaxConcurrentCaptures; n > 0 {
		return int64(n)
	}
	return defaultArchiveConcurrentCaps
}

// acquireCapture reserves a streaming capture slot. It returns false when the
// concurrent limit is reached; the caller then records an omission marker only.
func (a *RequestArchive) acquireCapture(limit int64) bool {
	for {
		n := a.activeCaptures.Load()
		if n >= limit {
			return false
		}
		if a.activeCaptures.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (a *RequestArchive) LogRequest(url, method string, headers map[string][]string, body []byte, status int, responseHeaders map[string][]string, response, ws, upstreamRequest, upstreamResponse, upstreamWS []byte, apiErrors []*interfaces.ErrorMessage, id string, started, received time.Time) error {
	return a.LogRequestWithOptions(url, method, headers, body, status, responseHeaders, response, ws, upstreamRequest, upstreamResponse, upstreamWS, apiErrors, false, id, started, received)
}

func (a *RequestArchive) LogRequestWithOptions(url, method string, headers map[string][]string, body []byte, status int, responseHeaders map[string][]string, response, ws, upstreamRequest, upstreamResponse, upstreamWS []byte, apiErrors []*interfaces.ErrorMessage, force bool, id string, started, received time.Time) error {
	if !a.IsEnabled() && !force {
		return nil
	}
	if a.startupErr != nil {
		return a.startupErr
	}
	cfg := a.cfg.Load()
	combinedHeaders := make(map[string][]string)
	for name, values := range headers {
		combinedHeaders[name] = append(combinedHeaders[name], values...)
	}
	for name, values := range responseHeaders {
		combinedHeaders[name] = append(combinedHeaders[name], values...)
	}
	r := a.redactor(combinedHeaders)
	decoded, err := a.file.decompressResponse(responseHeaders, response)
	if err != nil {
		decoded = []byte("[COMPRESSED PAYLOAD OMITTED: DECOMPRESSION FAILED]")
	}
	safeHeaders := r.Headers(responseHeaders)
	for name := range safeHeaders {
		if bytes.EqualFold([]byte(name), []byte("Content-Encoding")) {
			delete(safeHeaders, name)
		}
	}
	// Error structures may carry arbitrary upstream text; serialize and redact
	// them rather than passing pointers to the raw errors to the file writer.
	if len(apiErrors) > 0 {
		safeErrors := make([]map[string]any, 0, len(apiErrors))
		for _, item := range apiErrors {
			if item == nil {
				continue
			}
			message := ""
			if item.Error != nil {
				message = string(r.Bytes([]byte(item.Error.Error())))
			}
			safeErrors = append(safeErrors, map[string]any{"status": item.StatusCode, "error": message, "body": string(r.Bytes(item.Body)), "headers": r.Headers(item.Headers), "addon": r.Headers(item.Addon)})
		}
		errorsJSON, _ := json.Marshal(safeErrors)
		upstreamResponse = append(append(bytes.Clone(upstreamResponse), '\n'), errorsJSON...)
	}
	// Each body section is masked first, then charged to one per-request budget
	// so the persisted record stays bounded. Metadata-only keeps URL, headers,
	// status and timing, and replaces every body with a marker.
	budget := archiveBudget{remaining: captureBudgetBytes(cfg)}
	metadataOnly := cfg.RequestArchive.MetadataOnly
	bounded := func(data []byte) []byte {
		if len(data) == 0 {
			return data
		}
		if metadataOnly {
			return []byte(archiveMetadataOnlyMarker)
		}
		return admitRedacted(&budget, data, r.secrets, archiveTruncatedMarker)
	}
	a.retentionMu.Lock()
	defer a.retentionMu.Unlock()
	err = a.file.LogRequest(string(r.Bytes([]byte(url))), method, r.Headers(headers), bounded(r.Bytes(body)), status, safeHeaders, bounded(r.Transcript(decoded)), bounded(r.Transcript(ws)), bounded(r.Bytes(upstreamRequest)), bounded(r.Transcript(upstreamResponse)), bounded(r.Transcript(upstreamWS)), nil, string(r.Bytes([]byte(id))), started, received)
	if err != nil {
		return err
	}
	return a.retainLocked()
}

func (a *RequestArchive) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, id string) (StreamingLogWriter, error) {
	if !a.IsEnabled() {
		return &NoOpStreamingLogWriter{}, nil
	}
	if a.startupErr != nil {
		return nil, a.startupErr
	}
	cfg := a.cfg.Load()
	r := a.redactor(headers)
	counted := a.acquireCapture(concurrentCaptureLimit(cfg))
	stream := &archiveStream{redactor: r, archive: a, counted: counted, budget: archiveBudget{remaining: captureBudgetBytes(cfg)}, limitMarker: archiveTruncatedMarker}
	if !counted {
		// Over the concurrent limit the request is still recorded, without transcript bytes.
		stream.budget.remaining = 0
		stream.pendingCut = true
		stream.limitMarker = archiveConcurrencyMarker
	}
	if cfg.RequestArchive.MetadataOnly {
		stream.budget.remaining = 0
		stream.metadataOnly = true
		stream.limitMarker = archiveMetadataOnlyMarker
	}
	// The file writer persists the body as soon as it is constructed, so the body is
	// admitted under the same rules as later sections before that call.
	admittedBody := stream.admitRequestBody(body)
	w, err := a.file.LogStreamingRequest(string(r.Bytes([]byte(url))), method, r.Headers(headers), admittedBody, string(r.Bytes([]byte(id))))
	if err != nil {
		if counted {
			a.activeCaptures.Add(-1)
		}
		return nil, err
	}
	if file, ok := w.(*FileStreamingLogWriter); ok {
		file.lossless = true
	}
	stream.StreamingLogWriter = w
	return stream, nil
}

type archiveStream struct {
	StreamingLogWriter
	redactor *ArchiveRedactor
	archive  *RequestArchive
	pending  []byte
	budget   archiveBudget
	// pendingCut records that raw chunks were dropped at the budget or concurrency limit.
	pendingCut   bool
	counted      bool
	metadataOnly bool
	limitMarker  string
}

func (w *archiveStream) WriteChunkAsync(chunk []byte) {
	// SSE deltas can split credentials between JSON records. Keep the wire
	// transcript in memory until final redaction; only sanitized bytes spool.
	kept, cut := w.budget.take(chunk)
	w.pending = append(w.pending, kept...)
	if cut {
		w.pendingCut = true
	}
}
func (w *archiveStream) WriteStatus(status int, headers map[string][]string) error {
	w.redactor = NewArchiveRedactor(append(w.redactor.secrets, w.archive.redactor(headers).secrets...))
	return w.StreamingLogWriter.WriteStatus(status, w.redactor.Headers(headers))
}
func (w *archiveStream) WriteAPIRequest(body []byte) error {
	return w.StreamingLogWriter.WriteAPIRequest(w.admit(body))
}
func (w *archiveStream) WriteAPIResponse(body []byte) error {
	return w.StreamingLogWriter.WriteAPIResponse(w.admit(body))
}
func (w *archiveStream) WriteAPIWebsocketTimeline(body []byte) error {
	return w.StreamingLogWriter.WriteAPIWebsocketTimeline(w.admit(body))
}

// admit masks an upstream section and charges it to the same budget as chunks.
func (w *archiveStream) admit(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	if w.metadataOnly {
		return []byte(archiveMetadataOnlyMarker)
	}
	masked := w.redactor.Transcript(body)
	return admitRedacted(&w.budget, masked, w.redactor.secrets, w.limitMarker)
}

// admitRequestBody masks the client request body and charges it to the same budget.
func (w *archiveStream) admitRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	if w.metadataOnly {
		return []byte(archiveMetadataOnlyMarker)
	}
	return admitRedacted(&w.budget, w.redactor.Bytes(body), w.redactor.secrets, w.limitMarker)
}

func (w *archiveStream) Close() error {
	if w.counted {
		defer w.archive.activeCaptures.Add(-1)
	}
	w.redactor = NewArchiveRedactor(append(w.redactor.secrets, w.archive.redactor(nil).secrets...))
	if len(w.pending) > 0 || w.pendingCut {
		chunk := w.redactor.Transcript(w.pending)
		if w.pendingCut {
			chunk = markCut(chunk, w.redactor.secrets, w.limitMarker)
		}
		if len(chunk) > 0 {
			w.StreamingLogWriter.WriteChunkAsync(chunk)
		}
		w.pending = nil
	}
	w.archive.retentionMu.Lock()
	defer w.archive.retentionMu.Unlock()
	if err := w.StreamingLogWriter.Close(); err != nil {
		return err
	}
	return w.archive.retainLocked()
}

var _ RequestLogger = (*RequestArchive)(nil)
