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
}

func NewRequestArchive(cfg *config.Config, configDir, logsDir string) *RequestArchive {
	dir := cfg.RequestArchive.Directory
	if dir == "" {
		dir = filepath.Join(logsDir, "requests")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(configDir, dir)
	}
	a := &RequestArchive{file: NewFileRequestLogger(true, dir, "", 0)}
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
	gb := cfg.RequestArchive.MaxSizeGB
	if gb <= 0 {
		gb = 30
	}
	_, err := enforceLogDirSizeLimit(a.file.logsDir, int64(gb)*1000000000, "")
	return err
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
	a.retentionMu.Lock()
	defer a.retentionMu.Unlock()
	err = a.file.LogRequest(string(r.Bytes([]byte(url))), method, r.Headers(headers), r.Bytes(body), status, safeHeaders, r.Transcript(decoded), r.Transcript(ws), r.Bytes(upstreamRequest), r.Transcript(upstreamResponse), r.Transcript(upstreamWS), nil, string(r.Bytes([]byte(id))), started, received)
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
	r := a.redactor(headers)
	w, err := a.file.LogStreamingRequest(string(r.Bytes([]byte(url))), method, r.Headers(headers), r.Bytes(body), string(r.Bytes([]byte(id))))
	if err != nil {
		return nil, err
	}
	if file, ok := w.(*FileStreamingLogWriter); ok {
		file.lossless = true
	}
	return &archiveStream{StreamingLogWriter: w, redactor: r, archive: a}, nil
}

type archiveStream struct {
	StreamingLogWriter
	redactor *ArchiveRedactor
	archive  *RequestArchive
	pending  []byte
}

func (w *archiveStream) WriteChunkAsync(chunk []byte) {
	// SSE deltas can split credentials between JSON records. Keep the wire
	// transcript in memory until final redaction; only sanitized bytes spool.
	w.pending = append(w.pending, chunk...)
}
func (w *archiveStream) WriteStatus(status int, headers map[string][]string) error {
	w.redactor = NewArchiveRedactor(append(w.redactor.secrets, w.archive.redactor(headers).secrets...))
	return w.StreamingLogWriter.WriteStatus(status, w.redactor.Headers(headers))
}
func (w *archiveStream) WriteAPIRequest(body []byte) error {
	return w.StreamingLogWriter.WriteAPIRequest(w.redactor.Transcript(body))
}
func (w *archiveStream) WriteAPIResponse(body []byte) error {
	return w.StreamingLogWriter.WriteAPIResponse(w.redactor.Transcript(body))
}
func (w *archiveStream) WriteAPIWebsocketTimeline(body []byte) error {
	return w.StreamingLogWriter.WriteAPIWebsocketTimeline(w.redactor.Transcript(body))
}
func (w *archiveStream) Close() error {
	w.redactor = NewArchiveRedactor(append(w.redactor.secrets, w.archive.redactor(nil).secrets...))
	if len(w.pending) > 0 {
		w.StreamingLogWriter.WriteChunkAsync(w.redactor.Transcript(w.pending))
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
